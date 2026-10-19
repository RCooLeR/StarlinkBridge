package cloud

// A persisted wrapper around Go's RFC 6265 jar. The standard jar remains the
// authority for acceptance and request matching; records retain scope and expiry
// because CookieJar.Cookies deliberately strips these attributes.
import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

type storedCookie struct {
	Name     string        `json:"name"`
	Value    string        `json:"value"`
	Domain   string        `json:"domain"`
	Path     string        `json:"path"`
	HostOnly bool          `json:"host_only"`
	Secure   bool          `json:"secure"`
	HTTPOnly bool          `json:"http_only"`
	SameSite http.SameSite `json:"same_site"`
	Expires  time.Time     `json:"expires,omitempty"`
	MaxAge   int           `json:"max_age,omitempty"`
	Created  time.Time     `json:"created"`
	Origin   string        `json:"origin"`
}

type jarState struct {
	Version       int                      `json:"version"`
	BootstrapHash string                   `json:"bootstrap_hash,omitempty"`
	LastRefresh   time.Time                `json:"last_refresh,omitempty"`
	Cookies       []storedCookie           `json:"cookies"`
	AuthScopes    map[string]authAuthority `json:"auth_scopes,omitempty"`
}

type persistentJar struct {
	mu            sync.Mutex
	jar           *cookiejar.Jar
	path          string
	records       map[string]storedCookie
	bootstrapHash string
	lastRefresh   time.Time
	persistErr    error
	authScopes    map[string]authAuthority
}

func newJar(path string) (*persistentJar, error) {
	j := &persistentJar{path: path, records: make(map[string]storedCookie), authScopes: make(map[string]authAuthority)}
	j.jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, errors.New("cannot read cloud cookie state")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid cloud cookie state file")
	}
	var state jarState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 {
		return nil, errors.New("invalid cloud cookie state file")
	}
	j.bootstrapHash, j.lastRefresh = state.BootstrapHash, state.LastRefresh
	for name, authority := range state.AuthScopes {
		if !validAuthAuthority(name, authority) {
			return nil, errors.New("invalid authentication cookie scope in cloud state")
		}
		j.authScopes[name] = authority
	}
	sort.SliceStable(state.Cookies, func(a, b int) bool { return state.Cookies[a].Created.Before(state.Cookies[b].Created) })
	for _, r := range state.Cookies {
		u, e := url.Parse(r.Origin)
		if e != nil || !allowedURL(u) {
			return nil, errors.New("invalid cookie origin in cloud state")
		}
		// Older state files already contain the successfully rotated API cookies.
		// Infer authority from response origin, including expired records, so an
		// old indefinite bootstrap cannot become active again after expiry.
		j.inferAuthAuthority(r)
		if !r.Expires.IsZero() && !r.Expires.After(time.Now()) {
			continue
		}
		c := r.cookie()
		if normalized, ok := normalizeCookie(u, c, time.Now()); !ok || normalized.Domain != r.Domain || normalized.Path != r.Path || normalized.HostOnly != r.HostOnly {
			return nil, errors.New("invalid cookie scope in cloud state")
		}
		j.jar.SetCookies(u, []*http.Cookie{c})
		j.records[cookieKey(r)] = r
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			return nil, errors.New("cannot restrict cloud cookie state permissions")
		}
	}
	return j, nil
}

func (r storedCookie) cookie() *http.Cookie {
	domain := r.Domain
	if r.HostOnly {
		domain = ""
	}
	// MaxAge was converted to an absolute expiry on receipt. Replaying the
	// original relative MaxAge here would extend credentials at every restart.
	return &http.Cookie{Name: r.Name, Value: r.Value, Domain: domain, Path: r.Path, Secure: r.Secure, HttpOnly: r.HTTPOnly, SameSite: r.SameSite, Expires: r.Expires}
}

func cookieKey(r storedCookie) string { return r.Domain + "\x00" + r.Path + "\x00" + r.Name }

func normalizeCookie(u *url.URL, c *http.Cookie, now time.Time) (storedCookie, bool) {
	if !allowedURL(u) || c == nil || c.Name == "" || c.Valid() != nil {
		return storedCookie{}, false
	}
	path := c.Path
	if path == "" || path[0] != '/' {
		path = u.Path
		if path == "" || path[0] != '/' {
			path = "/"
		} else if i := strings.LastIndex(path, "/"); i <= 0 {
			path = "/"
		} else {
			path = path[:i]
		}
	}
	// Ask the standard jar whether this cookie's scope is valid. Temporarily
	// neutralize expiry so a legitimate deletion can be validated as well.
	probe, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	cp := *c
	cp.Path, cp.MaxAge, cp.Expires = path, 0, now.Add(time.Hour)
	probe.SetCookies(u, []*http.Cookie{&cp})
	target := *u
	target.Path, target.RawPath = path, ""
	accepted := false
	for _, x := range probe.Cookies(&target) {
		if x.Name == c.Name && x.Value == c.Value {
			accepted = true
			break
		}
	}
	if !accepted {
		return storedCookie{}, false
	}
	domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	if domain == "" {
		domain = strings.ToLower(u.Hostname())
	}
	expires := c.Expires
	if c.MaxAge > 0 {
		// Avoid duration overflow on an unusually large Max-Age.
		const maxSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if int64(c.MaxAge) > maxSeconds {
			expires = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		} else {
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
	}
	if c.MaxAge < 0 {
		expires = now.Add(-time.Second)
	}
	return storedCookie{Name: c.Name, Value: c.Value, Domain: domain, Path: path, HostOnly: c.Domain == "", Secure: c.Secure, HTTPOnly: c.HttpOnly, SameSite: c.SameSite, Expires: expires, MaxAge: c.MaxAge, Created: now, Origin: u.Scheme + "://" + u.Host + u.Path}, true
}

func (j *persistentJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !allowedURL(u) {
		return nil
	}
	return j.authCookiesLocked(u, j.jar.Cookies(u))
}

func (j *persistentJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !allowedURL(u) {
		return
	}
	now, changed := time.Now(), false
	if j.prepareAuthUpdatesLocked(u, cookies, now) {
		changed = true
	}
	accepted := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		r, ok := normalizeCookie(u, c, now)
		if !ok {
			continue
		}
		accepted = append(accepted, c)
		key := cookieKey(r)
		if !r.Expires.IsZero() && !r.Expires.After(now) {
			if _, exists := j.records[key]; exists {
				delete(j.records, key)
				changed = true
			}
		} else {
			if old, exists := j.records[key]; exists {
				r.Created = old.Created
			}
			if old, exists := j.records[key]; !exists || old != r {
				j.records[key] = r
				changed = true
			}
		}
	}
	j.jar.SetCookies(u, accepted)
	if changed {
		j.persistErr = j.saveLocked()
	}
}

func (j *persistentJar) replaceBootstrap(cookies []*http.Cookie, hash string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	oldJar, oldRecords, oldHash, oldRefresh, oldErr, oldAuth := j.jar, j.records, j.bootstrapHash, j.lastRefresh, j.persistErr, j.authScopes
	committed := false
	defer func() {
		if !committed {
			j.jar, j.records, j.bootstrapHash, j.lastRefresh, j.persistErr, j.authScopes = oldJar, oldRecords, oldHash, oldRefresh, oldErr, oldAuth
		}
	}()
	j.jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	j.records = make(map[string]storedCookie)
	j.authScopes = make(map[string]authAuthority)
	j.bootstrapHash, j.lastRefresh = hash, time.Time{}
	u, _ := url.Parse("https://api.starlink.com/")
	for _, c := range cookies {
		r, ok := normalizeCookie(u, c, time.Now())
		if !ok {
			return errors.New("invalid bootstrap cookie")
		}
		j.records[cookieKey(r)] = r
	}
	j.jar.SetCookies(u, cookies)
	j.persistErr = j.saveLocked()
	committed = j.persistErr == nil
	return j.persistErr
}

func (j *persistentJar) stateInfo() (string, time.Time, time.Time, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var earliest time.Time
	active := false
	for _, r := range j.records {
		if !r.Expires.IsZero() && !r.Expires.After(time.Now()) {
			continue
		}
		active = true
		if !r.Expires.IsZero() && (earliest.IsZero() || r.Expires.Before(earliest)) {
			earliest = r.Expires
		}
	}
	return j.bootstrapHash, j.lastRefresh, earliest, active
}

func (j *persistentJar) refreshed(now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lastRefresh = now
	j.persistErr = j.saveLocked()
	return j.persistErr
}

func (j *persistentJar) persistenceError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.persistErr
}

func (j *persistentJar) retryPersistence() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.persistErr != nil {
		j.persistErr = j.saveLocked()
	}
	return j.persistErr
}

func (j *persistentJar) saveLocked() error {
	state := jarState{Version: 1, BootstrapHash: j.bootstrapHash, LastRefresh: j.lastRefresh, AuthScopes: j.authScopes}
	for key, r := range j.records {
		if !r.Expires.IsZero() && !r.Expires.After(time.Now()) {
			delete(j.records, key)
			continue
		}
		state.Cookies = append(state.Cookies, r)
	}
	sort.Slice(state.Cookies, func(a, b int) bool { return cookieKey(state.Cookies[a]) < cookieKey(state.Cookies[b]) })
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil || atomicSecretWrite(j.path, data) != nil {
		return errors.New("cannot persist cloud cookie state")
	}
	return nil
}

func atomicSecretWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cookies-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return err
		}
	}
	return nil
}
