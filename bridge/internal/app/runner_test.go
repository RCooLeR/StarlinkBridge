package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/config"
	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
)

type fakeFetcher struct {
	snapshot model.Snapshot
	err      error
	calls    int
}

func (f *fakeFetcher) Fetch(context.Context) (model.Snapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

type fakeSession struct{ calls int }

func (f *fakeSession) EnsureSession(context.Context) error { f.calls++; return nil }
func (f *fakeSession) Health() model.Health                { return model.Health{CookieLastRefresh: time.Now()} }
func snap(id string) model.Snapshot {
	return model.Snapshot{DeviceID: id, CollectedAt: time.Now(), Values: map[string]float64{"power_watts": 33}, States: map[string]string{}}
}

func TestAutoFallbackIdentityAndRecovery(t *testing.T) {
	local := &fakeFetcher{err: errors.New("unreachable")}
	cloud := &fakeFetcher{snapshot: snap("ut123")}
	session := &fakeSession{}
	store := telemetry.NewStore(time.Minute)
	r := Runner{Config: config.Config{Mode: "auto", Timeout: time.Second}, Local: local, Cloud: cloud, Session: session, Store: store}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.View(time.Now()).Snapshot.Source != "cloud" {
		t.Fatal("did not fall back")
	}
	local.err = nil
	local.snapshot = snap("123")
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.View(time.Now()).Snapshot.Source != "local" || session.calls != 1 || cloud.calls != 1 {
		t.Fatal("local recovery or backup session renewal broken")
	}
	local.snapshot = snap("ut-other")
	cloud.snapshot = snap("ut-other")
	if err := r.Poll(context.Background()); err == nil {
		t.Fatal("allowed device identity change")
	}
	if store.View(time.Now()).Snapshot.DeviceID != "ut123" {
		t.Fatal("overwrote original device")
	}
}

func TestReadinessAndMetricsExpire(t *testing.T) {
	store := telemetry.NewStore(time.Second)
	s := snap("ut123")
	s.CollectedAt = time.Now().Add(-2 * time.Second)
	store.Update(&s, model.Health{LastSuccess: s.CollectedAt})
	handler := Handler(store)
	for _, test := range []struct {
		path string
		code int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/api/v1/status", 200}, {"/metrics", 200}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code {
			t.Fatalf("%s: %d", test.path, response.Code)
		}
		if test.path == "/metrics" && strings.Contains(response.Body.String(), "starlink_power_watts{") {
			t.Fatal("stale power still exported")
		}
	}
}
