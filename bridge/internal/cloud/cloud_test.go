package cloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/protocol"
	"google.golang.org/protobuf/encoding/protowire"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func target(s string) *url.URL { u, _ := url.Parse(s); return u }
func cookieValue(j http.CookieJar, endpoint, name string) string {
	for _, c := range j.Cookies(target(endpoint)) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
func response(code int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}
func testConfig(t *testing.T, transport http.RoundTripper) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{CookieFile: filepath.Join(dir, "bootstrap"), StateFile: filepath.Join(dir, "state", "cookies.json"), transport: transport}
}
func writeBootstrap(t *testing.T, cfg Config, raw string) {
	t.Helper()
	if err := os.WriteFile(cfg.CookieFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestJarScopesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	j, err := newJar(path)
	if err != nil {
		t.Fatal(err)
	}
	j.SetCookies(target("https://api.starlink.com/auth-rp/auth/user"), []*http.Cookie{
		{Name: "wide", Value: "shared", Domain: ".starlink.com", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
		{Name: "api", Value: "host-only", Path: "/", Secure: true},
		{Name: "path", Value: "private", Secure: true}, // RFC default path is /auth-rp/auth.
		{Name: "expires", Value: "hour", Path: "/", MaxAge: 3600},
	})
	j.SetCookies(target("https://starlink.com/"), []*http.Cookie{{Name: "bare", Value: "only", Path: "/"}})
	j.SetCookies(target("https://www.starlink.com/"), []*http.Cookie{{Name: "www", Value: "only", Path: "/"}})
	if err = j.persistenceError(); err != nil {
		t.Fatal(err)
	}
	_, _, before, _ := j.stateInfo()
	reloaded, err := newJar(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, after, _ := reloaded.stateInfo()
	if !before.Equal(after) {
		t.Fatal("Max-Age was extended across restart")
	}
	for _, host := range []string{"starlink.com", "www.starlink.com", "api.starlink.com"} {
		if cookieValue(reloaded, "https://"+host+"/", "wide") != "shared" {
			t.Fatalf("domain cookie missing at %s", host)
		}
	}
	if cookieValue(reloaded, "https://starlink.com/", "api") != "" || cookieValue(reloaded, "https://www.starlink.com/", "bare") != "" {
		t.Fatal("host-only scope widened")
	}
	if cookieValue(reloaded, "https://api.starlink.com/auth-rp/auth/user", "path") != "private" || cookieValue(reloaded, "https://api.starlink.com/auth-rp/authentication", "path") != "" {
		t.Fatal("default path matching incorrect")
	}
	if len(reloaded.Cookies(target("https://other.starlink.com/"))) != 0 || len(reloaded.Cookies(target("http://api.starlink.com/"))) != 0 {
		t.Fatal("cookies escaped fixed HTTPS host boundary")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("state permissions are not 0600")
		}
	}
}

func TestCookieDeletionAndExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	j, _ := newJar(path)
	u := target("https://api.starlink.com/x/y")
	j.SetCookies(u, []*http.Cookie{{Name: "session", Value: "keep", Path: "/"}, {Name: "session", Value: "remove", Path: "/x"}, {Name: "stale", Value: "discard", Path: "/", Expires: time.Now().Add(time.Hour)}})
	j.SetCookies(u, []*http.Cookie{{Name: "session", Path: "/x", MaxAge: -1}, {Name: "stale", Path: "/", Expires: time.Now().Add(-time.Hour)}, {Name: "bad", Value: "bad", Domain: "example.com", Path: "/"}, {Name: "public", Value: "bad", Domain: "com", Path: "/"}})
	reloaded, err := newJar(path)
	if err != nil {
		t.Fatal(err)
	}
	cookies := reloaded.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Value != "keep" {
		t.Fatal("deletion, expired cookie, or invalid domain incorrectly retained")
	}
}

func TestBootstrapRotationPersistenceAndHotReplacement(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cookie, err := r.Cookie("session")
		if err != nil {
			t.Fatal(err)
		}
		want := "initial"
		if calls == 2 {
			want = "fresh"
		}
		if cookie.Value != want {
			t.Fatalf("bootstrap value %q want %q", cookie.Value, want)
		}
		if r.Header.Get("X-XSRF-Token") != "a+b c" {
			t.Fatal("XSRF token was not correctly URL decoded")
		}
		return response(200, `{"content":{}}`, http.Header{"Set-Cookie": []string{"session=rotated; Domain=.starlink.com; Path=/; Secure; HttpOnly; Max-Age=3600"}}), nil
	})
	cfg := testConfig(t, transport)
	writeBootstrap(t, cfg, "session=initial; XSRF-TOKEN=a%2Bb%20c\n")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cookieValue(c.jar, "https://www.starlink.com/", "session") != "rotated" {
		t.Fatal("rotated value unavailable on web host")
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || cookieValue(restarted.jar, authURL, "session") != "rotated" {
		t.Fatal("unchanged stale bootstrap overwrote persisted rotation")
	}
	writeBootstrap(t, cfg, "session=fresh; XSRF-TOKEN=a%2Bb%20c")
	if err = restarted.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || restarted.Health().AuthRequired {
		t.Fatal("new bootstrap was not immediately activated")
	}
	if restarted.Health().CookieLastRefresh.IsZero() || restarted.Health().CookieExpiresAt.IsZero() {
		t.Fatal("session health missing")
	}
}

func TestMissingBootstrapCanAppearAfterStartup(t *testing.T) {
	calls := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return response(200, `{}`, nil), nil }))
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.EnsureSession(context.Background()) == nil || !c.Health().AuthRequired || calls != 0 {
		t.Fatal("missing credentials not classified without network access")
	}
	writeBootstrap(t, cfg, "session=fresh")
	if err = c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.Health().AuthRequired {
		t.Fatal("client failed to recover")
	}
}

func TestEmptyBootstrapDoesNotErasePersistedSession(t *testing.T) {
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return response(200, `{}`, nil), nil }))
	writeBootstrap(t, cfg, "session=valid")
	c, _ := New(cfg)
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeBootstrap(t, cfg, "")
	c, _ = New(cfg)
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cookieValue(c.jar, authURL, "session") != "valid" {
		t.Fatal("empty placeholder erased stored session")
	}
}

func Test401CookiesAppliedBeforeSingleRefreshRetry(t *testing.T) {
	step := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		step++
		got, _ := r.Cookie("session")
		switch step {
		case 1:
			return response(200, `{}`, nil), nil
		case 2:
			return response(401, "secret response", http.Header{"Set-Cookie": []string{"session=rotated-on-401; Domain=starlink.com; Path=/; Secure"}}), nil
		case 3:
			if r.URL.String() != authURL || got.Value != "rotated-on-401" {
				t.Fatal("refresh missed 401 cookie rotation")
			}
			return response(200, `{}`, http.Header{"Set-Cookie": []string{"session=renewed; Domain=starlink.com; Path=/; Secure"}}), nil
		case 4:
			if got.Value != "renewed" {
				t.Fatal("retry missed refreshed cookie")
			}
			return response(401, "secret response", nil), nil
		default:
			t.Fatal("unbounded authentication retries")
		}
		return nil, errors.New("unexpected")
	}))
	writeBootstrap(t, cfg, "session=initial")
	c, _ := New(cfg)
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.request(context.Background(), http.MethodGet, serviceLinesURL, nil)
	if err == nil || strings.Contains(err.Error(), "secret") || step != 4 || !c.Health().AuthRequired {
		t.Fatal("incorrect bounded retry/classification")
	}
	restarted, _ := New(cfg)
	if cookieValue(restarted.jar, authURL, "session") != "renewed" {
		t.Fatal("failed response rotations were not persisted")
	}
}

func TestRefreshBackoffIsResetByFreshBootstrap(t *testing.T) {
	calls := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return response(403, "private", nil), nil }))
	writeBootstrap(t, cfg, "session=bad")
	c, _ := New(cfg)
	for i := 0; i < 4; i++ {
		if c.EnsureSession(context.Background()) == nil {
			t.Fatal("403 accepted")
		}
	}
	if calls != 1 || !c.Health().AuthRequired {
		t.Fatal("failed session checks did not back off")
	}
	writeBootstrap(t, cfg, "session=new")
	_ = c.EnsureSession(context.Background())
	if calls != 2 {
		t.Fatal("fresh login did not bypass failed-renewal backoff")
	}
}

func TestRedirectCredentialBoundary(t *testing.T) {
	calls := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(302, "private", http.Header{"Location": []string{"https://attacker.example/steal"}, "Set-Cookie": []string{"session=on-redirect; Domain=starlink.com; Path=/; Secure"}}), nil
	}))
	writeBootstrap(t, cfg, "session=secret")
	c, _ := New(cfg)
	err := c.EnsureSession(context.Background())
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatal("redirect followed or not classified")
	}
	if cookieValue(c.jar, authURL, "session") != "on-redirect" {
		t.Fatal("redirect cookie update lost")
	}
	if _, _, err = c.requestOnce(context.Background(), http.MethodGet, "https://attacker.example/", nil); err == nil || calls != 1 {
		t.Fatal("untrusted endpoint requested")
	}
}

func TestErrorsDoNotExposeSecrets(t *testing.T) {
	for name, rt := range map[string]roundTripFunc{
		"network": func(*http.Request) (*http.Response, error) { return nil, errors.New("secret-cookie") },
		"HTTP":    func(*http.Request) (*http.Response, error) { return response(500, "secret-cookie", nil), nil },
		"oversize": func(*http.Request) (*http.Response, error) {
			return response(200, strings.Repeat("x", maxResponse+1), nil), nil
		},
		"invalid auth": func(*http.Request) (*http.Response, error) { return response(200, "secret-cookie", nil), nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t, rt)
			writeBootstrap(t, cfg, "session=secret-cookie")
			c, _ := New(cfg)
			err := c.EnsureSession(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret-cookie") {
				t.Fatal("error missing or leaks private data")
			}
		})
	}
}

func TestInvalidBootstrapDoesNotReplaceGoodState(t *testing.T) {
	cfg := testConfig(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, `{}`, nil), nil }))
	writeBootstrap(t, cfg, "session=good")
	c, _ := New(cfg)
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeBootstrap(t, cfg, "session=bad\nInjected: secret")
	if err := c.EnsureSession(context.Background()); err == nil {
		t.Fatal("multiline cookie accepted")
	}
	if cookieValue(c.jar, authURL, "session") != "good" {
		t.Fatal("bad import destroyed working state")
	}
}

func TestBootstrapSaveFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	j, _ := newJar(path)
	good, _ := parseBootstrap("session=good")
	if err := j.replaceBootstrap(good, "original"); err != nil {
		t.Fatal(err)
	}
	j.path = filepath.Join(path, "impossible-child")
	bad, _ := parseBootstrap("session=new")
	if err := j.replaceBootstrap(bad, "new"); err == nil {
		t.Fatal("expected persistence failure")
	}
	hash, _, _, _ := j.stateInfo()
	if hash != "original" || cookieValue(j, authURL, "session") != "good" {
		t.Fatal("failed bootstrap was partially activated")
	}
}

func protoBytes(field protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), b)
}
func statusFixture(id string) []byte {
	return protoBytes(2004, protoBytes(1, protoBytes(1, []byte(id))))
}
func historyFixture() []byte {
	data := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)
	data = protowire.AppendFixed32(protowire.AppendTag(data, 1001, protowire.Fixed32Type), 0)
	data = protowire.AppendFixed32(protowire.AppendTag(data, 1010, protowire.Fixed32Type), math.Float32bits(32.5))
	return protoBytes(2006, data)
}

func TestFetchThroughHTTPServer(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !strings.Contains(r.Header.Get("Cookie"), "session=login") {
			t.Error("missing account session")
		}
		switch r.URL.Path {
		case "/auth-rp/auth/user":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"content":{}}`)
		case "/webagg/v2/accounts/service-lines":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"abc"}]}],"isLastPage":true}}`)
		case rpcPath:
			body, _ := io.ReadAll(r.Body)
			var data []byte
			if bytes.Equal(body, protocol.EncodeWebFrame(protocol.StatusRequest("utabc"))) {
				data = statusFixture("utabc")
			} else if bytes.Equal(body, protocol.EncodeWebFrame(protocol.HistoryRequest("utabc"))) {
				data = historyFixture()
			} else {
				t.Error("unexpected device operation")
			}
			w.Header().Set("Content-Type", "application/grpc-web+proto")
			w.Header().Set("Grpc-Status", "0")
			w.Write(protocol.EncodeWebFrame(data))
		default:
			t.Error("unexpected path")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host, u.Scheme = serverURL.Host, serverURL.Scheme
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	}))
	writeBootstrap(t, cfg, "session=login")
	c, _ := New(cfg)
	snapshot, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || snapshot.DeviceID != "utabc" || snapshot.Source != "cloud" || !snapshot.HistoryAvailable || snapshot.Values["power_watts"] != 32.5 {
		t.Fatalf("bad snapshot: requests=%d snapshot=%+v", requests, snapshot)
	}
	if c.Health().LastSuccess.IsZero() {
		t.Fatal("successful poll not reflected in health")
	}
}

func TestConfiguredDeviceMustBelongToAccount(t *testing.T) {
	calls := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() == authURL {
			return response(200, `{}`, nil), nil
		}
		return response(200, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"different"}]}],"isLastPage":true}}`, nil), nil
	}))
	cfg.DeviceID = "utwanted"
	writeBootstrap(t, cfg, "session=login")
	c, _ := New(cfg)
	_, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "configured dish") || calls != 2 {
		t.Fatal("wrong-account device was fetched")
	}
}

func TestDeviceResponseIdentityMismatchRejected(t *testing.T) {
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == authURL {
			return response(200, `{}`, nil), nil
		}
		if r.Method == http.MethodGet {
			return response(200, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"expected"}]}]}}`, nil), nil
		}
		return response(200, string(protocol.EncodeWebFrame(statusFixture("utwrong"))), http.Header{"Content-Type": []string{"application/grpc-web+proto"}, "Grpc-Status": []string{"0"}}), nil
	}))
	writeBootstrap(t, cfg, "session=login")
	c, _ := New(cfg)
	_, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unexpected device") {
		t.Fatal("device identity mismatch accepted")
	}
}

func TestGRPCAuthenticationUsesOneRefreshAndNeverLeakesServerText(t *testing.T) {
	for _, code := range []string{"16", "7"} {
		t.Run(code, func(t *testing.T) {
			authCalls, rpcCalls := 0, 0
			cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == authURL {
					authCalls++
					return response(200, `{}`, nil), nil
				}
				rpcCalls++
				trailer := protocol.EncodeWebFrame([]byte("grpc-status: " + code + "\r\ngrpc-message: secret-cookie\r\n"))
				trailer[0] = 128
				return response(200, string(trailer), http.Header{"Content-Type": []string{"application/grpc-web+proto"}}), nil
			}))
			writeBootstrap(t, cfg, "\ufeffsession=login\r\n")
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err = c.readDevice(context.Background(), protocol.StatusRequest("utabc"))
			want := 2
			if code == "7" {
				want = 1
			}
			if err == nil || strings.Contains(err.Error(), "secret-cookie") || authCalls != want || rpcCalls != want || !c.Health().AuthRequired {
				t.Fatal("gRPC auth failure not bounded/classified/sanitized")
			}
			if c.EnsureSession(context.Background()) == nil || authCalls != want {
				t.Fatal("rejected device authentication did not back off")
			}
		})
	}
}

func TestPeriodicRefreshIndependentOfFetch(t *testing.T) {
	calls := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return response(200, `{}`, nil), nil }))
	writeBootstrap(t, cfg, "session=login")
	c, _ := New(cfg)
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("session refreshed before interval elapsed")
	}
	if err := c.jar.refreshed(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("session not refreshed independently from metrics collection")
	}
}

func TestDiscoveryPaginationAndMultipleDeviceGuard(t *testing.T) {
	pages := 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == authURL {
			return response(200, `{}`, nil), nil
		}
		if r.Method != http.MethodGet {
			t.Fatal("device chosen from ambiguous account")
		}
		pages++
		if r.URL.Query().Get("page") == "0" {
			return response(200, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"one"}]}],"isLastPage":false}}`, nil), nil
		}
		return response(200, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"two"},{"userTerminalId":"one"}]}],"isLastPage":true}}`, nil), nil
	}))
	writeBootstrap(t, cfg, "session=login")
	c, _ := New(cfg)
	_, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple dishes") || pages != 2 {
		t.Fatal("pagination or ambiguous account check failed")
	}
}

func historyErrorClient(t *testing.T, denial string) (*Client, *int, *int) {
	t.Helper()
	statusCalls, historyCalls := 0, 0
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == authURL {
			return response(200, `{}`, nil), nil
		}
		if r.Method == http.MethodGet {
			return response(200, `{"content":{"results":[{"userTerminals":[{"userTerminalId":"abc"}]}]}}`, nil), nil
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Equal(body, protocol.EncodeWebFrame(protocol.StatusRequest("utabc"))) {
			statusCalls++
			return response(200, string(protocol.EncodeWebFrame(statusFixture("utabc"))), http.Header{"Content-Type": []string{"application/grpc-web+proto"}, "Grpc-Status": []string{"0"}}), nil
		}
		historyCalls++
		if denial == "HTTP403" {
			return response(403, "private", nil), nil
		}
		if denial == "HTTP401" {
			return response(401, "private", nil), nil
		}
		trailer := protocol.EncodeWebFrame([]byte("grpc-status: " + denial + "\r\n"))
		trailer[0] = 128
		return response(200, string(trailer), http.Header{"Content-Type": []string{"application/grpc-web+proto"}}), nil
	}))
	writeBootstrap(t, cfg, "session=login")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, &statusCalls, &historyCalls
}

func TestHistoryPermissionDenialDoesNotBlockStatus(t *testing.T) {
	for _, denial := range []string{"HTTP403", "7"} {
		t.Run(denial, func(t *testing.T) {
			c, statusCalls, historyCalls := historyErrorClient(t, denial)
			for i := 0; i < 2; i++ {
				snapshot, err := c.Fetch(context.Background())
				if err != nil {
					t.Fatal("optional history denied blocked valid status:", err)
				}
				if snapshot.HistoryAvailable || snapshot.HistoryError == "" || snapshot.DeviceID != "utabc" || c.Health().AuthRequired {
					t.Fatal("history permission error corrupted status/session health")
				}
			}
			if *statusCalls != 2 || *historyCalls != 1 {
				t.Fatal("status not polled again or denied history request hammered")
			}
			c.historyRetryAt = time.Now().Add(-time.Second)
			if _, err := c.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if *statusCalls != 3 || *historyCalls != 2 || time.Until(c.historyRetryAt) < 90*time.Second {
				t.Fatal("history permission retry did not back off independently")
			}
		})
	}
}

func TestHistoryUnauthenticatedStillRequiresGlobalRenewal(t *testing.T) {
	for _, denial := range []string{"HTTP401", "16"} {
		t.Run(denial, func(t *testing.T) {
			c, statusCalls, historyCalls := historyErrorClient(t, denial)
			snapshot, err := c.Fetch(context.Background())
			if err != nil || snapshot.HistoryError == "" || !c.Health().AuthRequired {
				t.Fatal("unauthenticated history did not retain status and require renewal")
			}
			if *statusCalls != 1 || *historyCalls != 2 {
				t.Fatal("history authentication retry was not bounded")
			}
			if _, err = c.Fetch(context.Background()); err == nil || *statusCalls != 1 {
				t.Fatal("genuine authentication rejection failed to gate session retry")
			}
		})
	}
}
