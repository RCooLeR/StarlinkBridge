package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func accessCookies(marker string, chunks ...string) []*http.Cookie {
	cookies := []*http.Cookie{{Name: accessCookie, Value: marker, Path: "/", Secure: true, HttpOnly: true, MaxAge: 3600}}
	for i, value := range chunks {
		cookies = append(cookies, &http.Cookie{Name: accessCookie + "C" + string(rune('1'+i)), Value: value, Path: "/", Secure: true, HttpOnly: true, MaxAge: 3600})
	}
	return cookies
}

func authJar(t *testing.T) *persistentJar {
	t.Helper()
	j, err := newJar(filepath.Join(t.TempDir(), "cookies.json"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := parseBootstrap(accessCookie + "=chunks-2; " + accessCookie + "C1=old-one; " + accessCookie + "C2=old-two; " + ssoCookie + "=old-sso; other=shared")
	if err != nil {
		t.Fatal(err)
	}
	if err = j.replaceBootstrap(bootstrap, "original"); err != nil {
		t.Fatal(err)
	}
	return j
}

func assertAccess(t *testing.T, j *persistentJar, endpoint string, want map[string]string) {
	t.Helper()
	got := make(map[string]string)
	for _, cookie := range j.Cookies(target(endpoint)) {
		if authCookieFamily(cookie.Name) != accessCookie {
			continue
		}
		if _, exists := got[cookie.Name]; exists {
			t.Fatal("duplicate access cookie name")
		}
		got[cookie.Name] = cookie.Value
	}
	if len(got) != len(want) {
		t.Fatalf("wrong access family size: got %d want %d", len(got), len(want))
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatal("wrong access generation or missing chunk")
		}
	}
}

func TestAuthCookieHandoffUsesOnlyCompleteFamilyAtFixedEndpoints(t *testing.T) {
	j := authJar(t)
	rotated := accessCookies("chunks-2", "new-one", "new-two")
	rotated = append(rotated, &http.Cookie{Name: "private-api", Value: "not-for-web", Path: "/", Secure: true}, &http.Cookie{Name: ssoCookie, Value: "new-sso", Path: "/", Secure: true})
	j.SetCookies(target(authURL), rotated)
	want := map[string]string{accessCookie: "chunks-2", accessCookie + "C1": "new-one", accessCookie + "C2": "new-two"}
	for _, endpoint := range []string{authURL, serviceLinesURL, "https://starlink.com" + rpcPath, "https://www.starlink.com" + rpcPath} {
		assertAccess(t, j, endpoint, want)
	}
	for _, endpoint := range []string{"https://starlink.com" + rpcPath, "https://www.starlink.com" + rpcPath} {
		if cookieValue(j, endpoint, "private-api") != "" || cookieValue(j, endpoint, ssoCookie) != "old-sso" {
			t.Fatal("unrelated API host-only cookie crossed to web host")
		}
	}
	if cookieValue(j, authURL, ssoCookie) != "new-sso" {
		t.Fatal("API SSO bootstrap shadow not replaced")
	}
	for _, endpoint := range []string{"https://starlink.com/account/home", "https://www.starlink.com/other", "https://starlink.com" + rpcPath + "/other"} {
		if cookieValue(j, endpoint, accessCookie+"C1") != "old-one" {
			t.Fatal("handoff escaped exact RPC path")
		}
	}
	for _, endpoint := range []string{"http://starlink.com" + rpcPath, "https://other.starlink.com" + rpcPath, "https://example.com" + rpcPath} {
		if len(j.Cookies(target(endpoint))) != 0 {
			t.Fatal("credentials escaped fixed HTTPS hosts")
		}
	}
}

func TestLegacyStateRecoversRotatedFamilyWithoutReimport(t *testing.T) {
	cfg := testConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return response(200, `{}`, nil), nil }))
	header := accessCookie + "=chunks-2; " + accessCookie + "C1=old-one; " + accessCookie + "C2=old-two"
	writeBootstrap(t, cfg, header)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.importBootstrap(); err != nil {
		t.Fatal(err)
	}
	c.jar.SetCookies(target(authURL), accessCookies("chunks-2", "rotated-one", "rotated-two"))
	if err = c.jar.refreshed(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Remove only the new authority metadata to recreate an old v1 state file.
	data, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var legacy jarState
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.AuthScopes = nil
	data, _ = json.Marshal(legacy)
	if err = os.WriteFile(cfg.StateFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAccess(t, restarted.jar, "https://starlink.com"+rpcPath, map[string]string{accessCookie: "chunks-2", accessCookie + "C1": "rotated-one", accessCookie + "C2": "rotated-two"})
	if err = restarted.jar.refreshed(time.Now()); err != nil {
		t.Fatal(err)
	}
	again, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertAccess(t, again.jar, authURL, map[string]string{accessCookie: "chunks-2", accessCookie + "C1": "rotated-one", accessCookie + "C2": "rotated-two"})
}

func TestAuthFamilyDeletionExpiryAndIncompleteUpdateNeverReviveBootstrap(t *testing.T) {
	for _, mode := range []string{"delete", "expire", "missing chunk", "missing marker", "invalid count"} {
		t.Run(mode, func(t *testing.T) {
			j := authJar(t)
			j.SetCookies(target(authURL), accessCookies("chunks-2", "valid-one", "valid-two"))
			var update []*http.Cookie
			switch mode {
			case "delete":
				update = []*http.Cookie{{Name: accessCookie, Path: "/", MaxAge: -1}}
			case "expire":
				update = []*http.Cookie{{Name: accessCookie, Value: "expired", Path: "/", Expires: time.Now().Add(-time.Hour)}}
			case "missing chunk":
				update = accessCookies("chunks-2", "new-one")
			case "missing marker":
				update = []*http.Cookie{{Name: accessCookie + "C1", Value: "new-one", Path: "/", Secure: true}}
			case "invalid count":
				update = accessCookies("chunks-9999", "new-one")
			}
			j.SetCookies(target(authURL), update)
			assertAccess(t, j, authURL, nil)
			assertAccess(t, j, "https://starlink.com"+rpcPath, nil)
			restarted, err := newJar(j.path)
			if err != nil {
				t.Fatal(err)
			}
			assertAccess(t, restarted, "https://starlink.com"+rpcPath, nil)
		})
	}
}

func TestAuthChunkCountShrinksAndUnchunkedRotation(t *testing.T) {
	j := authJar(t)
	j.SetCookies(target(authURL), accessCookies("chunks-3", "one", "two", "three"))
	j.SetCookies(target(authURL), accessCookies("chunks-1", "new-one"))
	assertAccess(t, j, "https://starlink.com"+rpcPath, map[string]string{accessCookie: "chunks-1", accessCookie + "C1": "new-one"})
	j.SetCookies(target(authURL), accessCookies("whole-token"))
	assertAccess(t, j, "https://starlink.com"+rpcPath, map[string]string{accessCookie: "whole-token"})
}

func TestCookieFromUnrelatedResponseCannotBecomeAuthAuthority(t *testing.T) {
	j := authJar(t)
	j.SetCookies(target("https://api.starlink.com/unrelated"), accessCookies("chunks-2", "other-one", "other-two"))
	assertAccess(t, j, "https://starlink.com"+rpcPath, map[string]string{accessCookie: "chunks-2", accessCookie + "C1": "old-one", accessCookie + "C2": "old-two"})
	j.SetCookies(target(authURL), accessCookies("chunks-2", "real-one", "real-two"))
	assertAccess(t, j, "https://starlink.com"+rpcPath, map[string]string{accessCookie: "chunks-2", accessCookie + "C1": "real-one", accessCookie + "C2": "real-two"})
	for _, cookie := range j.Cookies(target(authURL)) {
		if strings.Contains(cookie.Value, "old-") && authCookieFamily(cookie.Name) == accessCookie {
			t.Fatal("stale bootstrap duplicate remains")
		}
	}
}
