package cloud

// Browser bootstrap headers lose their original cookie scope. Starlink's auth
// endpoint renews host-only api.starlink.com Access.V1 cookies, while its device
// RPC lives on starlink.com. Explicitly hand off only this known credential family
// between those fixed endpoints; ordinary cookies remain governed by RFC scope.
import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const accessCookie = "Starlink.Com.Access.V1"
const ssoCookie = "Starlink.Com.Sso"
const ssoCheckCookie = "Starlink.Com.Sso.CheckSession"

type authAuthority struct {
	Domain   string    `json:"domain"`
	Path     string    `json:"path"`
	HostOnly bool      `json:"host_only"`
	Observed time.Time `json:"observed"`
}

func authCookieFamily(name string) string {
	if name == accessCookie || name == ssoCookie || name == ssoCheckCookie {
		return name
	}
	chunk, ok := strings.CutPrefix(name, accessCookie+"C")
	if !ok || chunk == "" {
		return ""
	}
	for _, c := range chunk {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return accessCookie
}

func validAuthAuthority(name string, a authAuthority) bool {
	return (name == accessCookie || name == ssoCookie || name == ssoCheckCookie) && a.Path == "/" &&
		((a.Domain == "api.starlink.com") || (a.Domain == "starlink.com" && !a.HostOnly))
}

func authorityFor(r storedCookie, observed time.Time) (string, authAuthority, bool) {
	name := authCookieFamily(r.Name)
	a := authAuthority{Domain: r.Domain, Path: r.Path, HostOnly: r.HostOnly, Observed: observed}
	u, err := url.Parse(r.Origin)
	trusted := err == nil && u.String() == authURL && validAuthAuthority(name, a)
	return name, a, trusted
}

func (j *persistentJar) inferAuthAuthority(r storedCookie) {
	name, a, ok := authorityFor(r, r.Created)
	if !ok {
		return
	}
	previous, exists := j.authScopes[name]
	if !exists || a.Observed.After(previous.Observed) {
		j.authScopes[name] = a
	}
}

func sameAuthScope(r storedCookie, a authAuthority) bool {
	return r.Domain == a.Domain && r.Path == a.Path && r.HostOnly == a.HostOnly
}

func (j *persistentJar) prepareAuthUpdatesLocked(u *url.URL, cookies []*http.Cookie, now time.Time) bool {
	if u.String() != authURL {
		return false
	}
	updates := make(map[string]authAuthority)
	for _, cookie := range cookies {
		r, ok := normalizeCookie(u, cookie, now)
		if !ok {
			continue
		}
		name, a, ok := authorityFor(r, now)
		if !ok {
			continue
		}
		// The marker is authoritative if a malformed response mixes scopes.
		_, exists := updates[name]
		if !exists || cookie.Name == name {
			updates[name] = a
		}
	}
	for name, a := range updates {
		j.authScopes[name] = a
		if name != accessCookie {
			continue
		}
		// A chunked credential is one generation. Never join new chunks to old
		// leftovers, even when a response omits one or the chunk count shrinks.
		for key, r := range j.records {
			if authCookieFamily(r.Name) != accessCookie || !sameAuthScope(r, a) {
				continue
			}
			delete(j.records, key)
			origin, _ := url.Parse(r.Origin)
			deleted := r.cookie()
			deleted.MaxAge = -1
			j.jar.SetCookies(origin, []*http.Cookie{deleted})
		}
	}
	return len(updates) > 0
}

func (j *persistentJar) authRecordLocked(name string, a authAuthority, now time.Time) (storedCookie, bool) {
	r, ok := j.records[a.Domain+"\x00"+a.Path+"\x00"+name]
	if !ok || !sameAuthScope(r, a) || (!r.Expires.IsZero() && !r.Expires.After(now)) {
		return storedCookie{}, false
	}
	return r, true
}

func (j *persistentJar) selectedAuthLocked(name string, a authAuthority, now time.Time) []*http.Cookie {
	base, ok := j.authRecordLocked(name, a, now)
	if !ok {
		return nil
	}
	selected := []*http.Cookie{{Name: base.Name, Value: base.Value}}
	if name != accessCookie || !strings.HasPrefix(base.Value, "chunks-") {
		return selected
	}
	count, err := strconv.Atoi(strings.TrimPrefix(base.Value, "chunks-"))
	if err != nil || count < 1 || count > 64 {
		return nil
	}
	for i := 1; i <= count; i++ {
		r, ok := j.authRecordLocked(accessCookie+"C"+strconv.Itoa(i), a, now)
		if !ok {
			return nil
		}
		selected = append(selected, &http.Cookie{Name: r.Name, Value: r.Value})
	}
	return selected
}

func (j *persistentJar) authCookiesLocked(u *url.URL, scoped []*http.Cookie) []*http.Cookie {
	api := u.Host == "api.starlink.com" && (u.Path == "/auth-rp/auth/user" || u.Path == "/webagg/v2/accounts/service-lines")
	rpc := (u.Host == "starlink.com" || u.Host == "www.starlink.com") && u.Path == rpcPath
	if !api && !rpc {
		return scoped
	}
	names := []string{accessCookie}
	if api {
		names = append(names, ssoCookie, ssoCheckCookie)
	}
	for _, name := range names {
		a, authoritative := j.authScopes[name]
		if !authoritative {
			continue
		}
		filtered := make([]*http.Cookie, 0, len(scoped))
		for _, cookie := range scoped {
			if authCookieFamily(cookie.Name) != name {
				filtered = append(filtered, cookie)
			}
		}
		// No fallback on expiry, deletion, or incomplete chunks: that would
		// silently revive the stale bootstrap generation.
		scoped = append(filtered, j.selectedAuthLocked(name, a, time.Now())...)
	}
	return scoped
}
