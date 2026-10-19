// Package cloud reads the consumer Starlink API using a user-supplied browser
// session. It cannot mint a new session after Starlink revokes or expires one.
package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/protocol"
)

const (
	serviceLinesURL = "https://api.starlink.com/webagg/v2/accounts/service-lines"
	authURL         = "https://api.starlink.com/auth-rp/auth/user"
	rpcPath         = "/api/SpaceX.API.Device.Device/Handle"
	maxResponse     = 4 << 20
)

// Config supplies file paths, never raw credentials. CookieFile is a bootstrap
// Cookie request-header value; StateFile contains the sensitive rotating jar.
type Config struct {
	CookieFile      string
	StateFile       string
	WebHost         string
	DeviceID        string
	Timeout         time.Duration
	RefreshInterval time.Duration
	transport       http.RoundTripper // Tests only: production hosts cannot be overridden.
}

type Client struct {
	mu               sync.Mutex
	cfg              Config
	http             *http.Client
	jar              *persistentJar
	health           model.Health
	device           string
	nextAuthAttempt  time.Time
	authFailures     uint
	readAuthFailures uint
	authError        error
	historyRetryAt   time.Time
	historyFailures  uint
	historyError     error
}

func New(cfg Config) (*Client, error) {
	if cfg.CookieFile == "" || cfg.StateFile == "" {
		return nil, errors.New("cloud bootstrap and state file paths are required")
	}
	var pathErr error
	cfg.CookieFile, pathErr = filepath.Abs(cfg.CookieFile)
	if pathErr != nil {
		return nil, errors.New("invalid cloud bootstrap file path")
	}
	cfg.StateFile, pathErr = filepath.Abs(cfg.StateFile)
	if pathErr != nil {
		return nil, errors.New("invalid cloud state file path")
	}
	if cfg.CookieFile == cfg.StateFile || (runtime.GOOS == "windows" && strings.EqualFold(cfg.CookieFile, cfg.StateFile)) {
		return nil, errors.New("cloud bootstrap and state files must be separate")
	}
	if cfg.WebHost == "" {
		cfg.WebHost = "starlink.com"
	}
	if cfg.WebHost != "starlink.com" && cfg.WebHost != "www.starlink.com" {
		return nil, errors.New("cloud web host must be starlink.com or www.starlink.com")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("cloud timeout must be positive")
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Minute
	}
	if cfg.RefreshInterval < time.Minute {
		return nil, errors.New("cloud refresh interval must be at least one minute")
	}
	if cfg.DeviceID != "" {
		var err error
		cfg.DeviceID, err = normalizeDish(cfg.DeviceID)
		if err != nil {
			return nil, err
		}
	}
	jar, err := newJar(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, jar: jar, health: model.Health{Source: "cloud"}}
	c.http = &http.Client{Jar: jar, Timeout: cfg.Timeout, Transport: cfg.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

// Health never includes cookie values, account responses, or server error text.
// CookieExpiresAt is the earliest known persistent cookie expiry, not a promise
// that the overall login session remains usable until then.
func (c *Client) Health() model.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.health
	_, h.CookieLastRefresh, h.CookieExpiresAt, _ = c.jar.stateInfo()
	return h
}

// EnsureSession imports a changed bootstrap file and checks/renews the session
// only when due. Calling this every poll is safe, including when auto mode uses
// the local dish. Failed refreshes back off; a fresh bootstrap resets backoff.
func (c *Client) EnsureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.ensureSession(ctx)
	if err != nil {
		c.health.LastError = err.Error()
	}
	return err
}

func (c *Client) ensureSession(ctx context.Context) error {
	if err := c.importBootstrap(); err != nil {
		return err
	}
	if err := c.jar.retryPersistence(); err != nil {
		return err
	}
	_, last, expiry, active := c.jar.stateInfo()
	if !active {
		c.health.AuthRequired = true
		return errors.New("cloud login required: add a fresh Cookie header to the bootstrap file")
	}
	due := last.IsZero() || last.After(time.Now()) || time.Since(last) >= c.cfg.RefreshInterval
	// A server may give a shorter lifetime than the configured refresh period.
	if !expiry.IsZero() && time.Until(expiry) <= 2*time.Minute && time.Since(last) >= time.Minute {
		due = true
	}
	if due || c.health.AuthRequired {
		return c.refresh(ctx)
	}
	return nil
}

// Fetch performs the two read-only device operations. History failure does not
// discard valid current status; its sanitized error is attached to the snapshot.
func (c *Client) Fetch(ctx context.Context) (snapshot model.Snapshot, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastAttempt = time.Now().UTC()
	defer func() {
		if err != nil {
			c.health.PollErrors++
			c.health.LastError = err.Error()
		}
	}()
	if err = c.ensureSession(ctx); err != nil {
		return snapshot, err
	}
	if c.device == "" {
		var dishes []string
		dishes, err = c.discover(ctx)
		if err != nil {
			return snapshot, fmt.Errorf("cloud discovery: %w", err)
		}
		if c.cfg.DeviceID != "" {
			for _, d := range dishes {
				if d == c.cfg.DeviceID {
					c.device = d
					break
				}
			}
			if c.device == "" {
				return snapshot, errors.New("configured dish was not found in the signed-in account")
			}
		} else {
			if len(dishes) == 0 {
				return snapshot, errors.New("no dish found in the signed-in account")
			}
			if len(dishes) != 1 {
				return snapshot, errors.New("multiple dishes found; configure the intended device ID")
			}
			c.device = dishes[0]
		}
	}
	var raw []byte
	raw, err = c.readDevice(ctx, protocol.StatusRequest(c.device))
	if err != nil {
		return snapshot, fmt.Errorf("cloud dish status: %w", err)
	}
	snapshot, err = protocol.DecodeStatus(raw)
	if err != nil {
		return model.Snapshot{}, errors.New("invalid cloud device status response")
	}
	if snapshot.DeviceID != "" {
		var reported string
		reported, err = normalizeDish(snapshot.DeviceID)
		if err != nil || reported != c.device {
			return model.Snapshot{}, errors.New("cloud status returned an unexpected device")
		}
	}
	snapshot.DeviceID, snapshot.Source, snapshot.CollectedAt = c.device, "cloud", time.Now().UTC()
	raw, historyErr := c.readHistory(ctx, protocol.HistoryRequest(c.device))
	if historyErr == nil {
		if e := protocol.ApplyHistory(&snapshot, raw); e != nil {
			historyErr = errors.New("invalid cloud history response")
		}
	}
	if historyErr != nil {
		snapshot.HistoryAvailable = false
		snapshot.HistoryError = fmt.Sprintf("cloud dish history: %v", historyErr)
	}
	c.health.LastSuccess = snapshot.CollectedAt
	if snapshot.HistoryError == "" {
		c.health.LastError = ""
	} else {
		c.health.LastError = snapshot.HistoryError
	}
	return snapshot, nil
}

func (c *Client) importBootstrap() error {
	f, err := os.Open(c.cfg.CookieFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read cloud bootstrap cookie file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("invalid cloud bootstrap cookie file")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if raw == "" {
		return nil
	} // An empty deployment placeholder must not erase state.
	hash := sha256.Sum256([]byte(raw))
	fingerprint := hex.EncodeToString(hash[:])
	previous, _, _, _ := c.jar.stateInfo()
	if fingerprint == previous {
		return nil
	}
	cookies, err := parseBootstrap(raw)
	if err != nil {
		return err
	}
	if err = c.jar.replaceBootstrap(cookies, fingerprint); err != nil {
		return err
	}
	c.device = ""
	c.nextAuthAttempt = time.Time{}
	c.authFailures = 0
	c.readAuthFailures = 0
	c.authError = nil
	c.historyRetryAt = time.Time{}
	c.historyFailures = 0
	c.historyError = nil
	c.health.AuthRequired = false
	return nil
}

func parseBootstrap(raw string) ([]*http.Cookie, error) {
	if strings.HasPrefix(strings.ToLower(raw), "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	for _, b := range []byte(raw) {
		if b < 32 || b > 126 {
			return nil, errors.New("bootstrap must contain one Cookie request-header value")
		}
	}
	var cookies []*http.Cookie
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r := &http.Request{Header: http.Header{"Cookie": []string{part}}}
		parsed := r.Cookies()
		if len(parsed) != 1 || parsed[0].Valid() != nil {
			return nil, errors.New("invalid bootstrap Cookie header")
		}
		cookie := parsed[0]
		if seen[cookie.Name] {
			return nil, errors.New("bootstrap contains duplicate cookie names with unknown scope")
		}
		seen[cookie.Name] = true
		// Cookie request headers contain no scope metadata. Initial import uses
		// Starlink's shared domain so API and website can exchange this session.
		// Every subsequent Set-Cookie retains the server's exact scope instead.
		cookie.Domain = ".starlink.com"
		cookie.Path = "/"
		cookie.Secure = true
		cookies = append(cookies, cookie)
	}
	if len(cookies) == 0 {
		return nil, errors.New("bootstrap contains no cookies")
	}
	return cookies, nil
}

func (c *Client) refresh(ctx context.Context) error {
	if time.Now().Before(c.nextAuthAttempt) && c.authError != nil {
		return c.authError
	}
	body, headers, err := c.requestOnce(ctx, http.MethodGet, authURL, nil)
	if err == nil && (!strings.Contains(strings.ToLower(headers.Get("Content-Type")), "json") || !json.Valid(body)) {
		err = errors.New("cloud session check returned an unexpected format")
	}
	if err != nil {
		unauth, denied := authClassification(err)
		if unauth || denied {
			c.health.AuthRequired = true
		}
		c.authFailures++
		shift := c.authFailures - 1
		if shift > 5 {
			shift = 5
		}
		c.nextAuthAttempt = time.Now().Add(time.Minute * time.Duration(1<<shift))
		c.authError = fmt.Errorf("cloud session refresh: %w", err)
		return c.authError
	}
	c.health.AuthRequired = false
	c.authFailures = 0
	c.nextAuthAttempt = time.Time{}
	c.authError = nil
	return c.jar.refreshed(time.Now().UTC())
}

type responseError struct{ status int }

func (e responseError) Error() string {
	switch {
	case e.status == 401:
		return "cloud session rejected (HTTP 401): supply a fresh login cookie if renewal fails"
	case e.status == 403:
		return "cloud access denied or blocked (HTTP 403); login or service access needs attention"
	case e.status >= 300 && e.status < 400:
		return "cloud redirect refused; check the session and configured web host"
	default:
		return fmt.Sprintf("Starlink cloud returned HTTP %d", e.status)
	}
}

func allowedURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "starlink.com", "www.starlink.com", "api.starlink.com":
		return true
	}
	return false
}

func (c *Client) requestOnce(ctx context.Context, method, endpoint string, body []byte) ([]byte, http.Header, error) {
	u, err := url.Parse(endpoint)
	if err != nil || !allowedURL(u) {
		return nil, nil, errors.New("cloud endpoint is outside the permitted Starlink hosts")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("cannot construct cloud request")
	}
	origin := "https://" + c.cfg.WebHost
	req.Header.Set("User-Agent", "StarlinkBridge/1.0")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/account/home")
	req.Header.Set("Accept", "application/json")
	for _, cookie := range c.jar.Cookies(u) {
		if cookie.Name == "XSRF-TOKEN" {
			value, e := url.PathUnescape(cookie.Value)
			if e == nil && !strings.ContainsAny(value, "\r\n") {
				req.Header.Set("X-XSRF-Token", value)
			}
			break
		}
	}
	if body != nil {
		req.Header.Set("Accept", "application/grpc-web+proto")
		req.Header.Set("Content-Type", "application/grpc-web+proto")
		req.Header.Set("X-Grpc-Web", "1")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, errors.New("could not reach Starlink cloud (network, TLS, or timeout)")
	}
	defer resp.Body.Close()
	// net/http processes all response Set-Cookie headers before returning,
	// including 401/403/redirect responses. Persist errors are never hidden.
	if err := c.jar.persistenceError(); err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, responseError{status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, nil, errors.New("could not read Starlink cloud response")
	}
	if len(data) > maxResponse {
		return nil, nil, errors.New("Starlink cloud response exceeded the size limit")
	}
	return data, resp.Header, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, http.Header, error) {
	data, h, err := c.requestOnce(ctx, method, endpoint, body)
	var status responseError
	if errors.As(err, &status) && status.status == 401 {
		c.health.AuthRequired = true
		if refreshErr := c.refresh(ctx); refreshErr != nil {
			return nil, nil, refreshErr
		}
		data, h, err = c.requestOnce(ctx, method, endpoint, body)
	}
	if errors.As(err, &status) && (status.status == 401 || status.status == 403) {
		c.backoffReadAuth(err)
	}
	return data, h, err
}

func (c *Client) readDevice(ctx context.Context, raw []byte) ([]byte, error) {
	return c.readOperation(ctx, raw, false)
}

func authClassification(err error) (unauth, denied bool) {
	var status responseError
	var rpc *protocol.RPCError
	unauth = (errors.As(err, &status) && status.status == 401) || (errors.As(err, &rpc) && rpc.Code == 16)
	denied = (errors.As(err, &status) && status.status == 403) || (errors.As(err, &rpc) && rpc.Code == 7)
	return
}

func (c *Client) readHistory(ctx context.Context, raw []byte) ([]byte, error) {
	if c.historyError != nil && time.Now().Before(c.historyRetryAt) {
		return nil, c.historyError
	}
	data, err := c.readOperation(ctx, raw, true)
	_, denied := authClassification(err)
	if denied {
		c.historyFailures++
		shift := c.historyFailures - 1
		if shift > 5 {
			shift = 5
		}
		c.historyRetryAt = time.Now().Add(time.Minute * time.Duration(1<<shift))
		c.historyError = err
	} else if err == nil {
		c.historyFailures = 0
		c.historyRetryAt = time.Time{}
		c.historyError = nil
	}
	return data, err
}

func (c *Client) readOperation(ctx context.Context, raw []byte, optionalHistory bool) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		data, err := c.readDeviceOnce(ctx, raw)
		if err == nil {
			c.readAuthFailures = 0
			return data, nil
		}
		unauth, denied := authClassification(err)
		if !unauth && !denied {
			return nil, err
		}
		// A method-specific permission denial must not poison the usable status
		// session. Genuine unauthenticated responses still require renewal.
		if denied && optionalHistory {
			return nil, err
		}
		c.health.AuthRequired = true
		if unauth && attempt == 0 {
			if refreshErr := c.refresh(ctx); refreshErr != nil {
				return nil, refreshErr
			}
			continue
		}
		c.backoffReadAuth(err)
		return nil, err
	}
	return nil, errors.New("cloud authentication retry exhausted")
}

func (c *Client) backoffReadAuth(err error) {
	c.readAuthFailures++
	shift := c.readAuthFailures - 1
	if shift > 5 {
		shift = 5
	}
	c.nextAuthAttempt = time.Now().Add(time.Minute * time.Duration(1<<shift))
	c.authError = err
	c.health.AuthRequired = true
}

func (c *Client) readDeviceOnce(ctx context.Context, raw []byte) ([]byte, error) {
	data, h, err := c.requestOnce(ctx, http.MethodPost, "https://"+c.cfg.WebHost+rpcPath, protocol.EncodeWebFrame(raw))
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(h.Get("Content-Type")), "application/grpc-web") {
		return nil, errors.New("cloud device response is not gRPC-Web (session may have expired)")
	}
	decoded, err := protocol.DecodeWebResponse(data, h)
	if err != nil {
		var rpc *protocol.RPCError
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, errors.New("cloud device RPC failed or returned an invalid frame")
	}
	return decoded, nil
}

var dishPattern = regexp.MustCompile(`^ut[A-Za-z0-9-]{1,128}$`)

func normalizeDish(id string) (string, error) {
	if !strings.HasPrefix(id, "ut") {
		id = "ut" + id
	}
	if !dishPattern.MatchString(id) {
		return "", errors.New("invalid Starlink user-terminal ID")
	}
	return id, nil
}

func (c *Client) discover(ctx context.Context) ([]string, error) {
	found := make(map[string]bool)
	for page := 0; page < 20; page++ {
		endpoint := fmt.Sprintf("%s?limit=100&page=%d&isConverting=false&onlyActive=false&onlyNoUts=false", serviceLinesURL, page)
		data, _, err := c.request(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Content *struct {
				Results *[]struct {
					UserTerminals []struct {
						UserTerminalID string `json:"userTerminalId"`
					} `json:"userTerminals"`
				} `json:"results"`
				IsLastPage *bool `json:"isLastPage"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &envelope) != nil || envelope.Content == nil || envelope.Content.Results == nil {
			return nil, errors.New("unexpected cloud service-line response")
		}
		rows := *envelope.Content.Results
		for _, line := range rows {
			for _, terminal := range line.UserTerminals {
				if terminal.UserTerminalID != "" {
					id, e := normalizeDish(terminal.UserTerminalID)
					if e != nil {
						return nil, e
					}
					found[id] = true
				}
			}
		}
		last := envelope.Content.IsLastPage
		if len(rows) == 0 || (last != nil && *last) || (last == nil && len(rows) < 100) {
			ids := make([]string, 0, len(found))
			for id := range found {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return ids, nil
		}
	}
	return nil, errors.New("cloud device discovery exceeded 20 pages")
}
