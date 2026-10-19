package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/config"
	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
)

type Fetcher interface {
	Fetch(context.Context) (model.Snapshot, error)
}
type Session interface {
	EnsureSession(context.Context) error
	Health() model.Health
}

type Runner struct {
	Config       config.Config
	Local, Cloud Fetcher
	Session      Session
	Store        *telemetry.Store
	Notify       func()
	health       model.Health
	deviceID     string
}

func (r *Runner) accept(snapshot *model.Snapshot) error {
	canonical, err := model.NormalizeDeviceID(snapshot.DeviceID)
	if err != nil {
		return err
	}
	snapshot.DeviceID = canonical
	want := r.deviceID
	if r.Config.DeviceID != "" {
		want, err = model.NormalizeDeviceID(r.Config.DeviceID)
		if err != nil {
			return err
		}
	}
	if snapshot.DeviceID == "" {
		return fmt.Errorf("dish response has no device ID")
	}
	if want != "" && want != snapshot.DeviceID {
		return fmt.Errorf("dish identity changed; check configured device ID")
	}
	r.deviceID = snapshot.DeviceID
	return nil
}

func (r *Runner) Poll(ctx context.Context) error {
	r.health.LastAttempt = time.Now().UTC()
	var snapshot model.Snapshot
	err := fmt.Errorf("no data source configured")
	if r.Config.Mode != "cloud" && r.Local != nil {
		snapshot, err = r.Local.Fetch(ctx)
		if err == nil {
			snapshot.Source = "local"
			err = r.accept(&snapshot)
		}
	}
	if r.Config.Mode == "cloud" || (r.Config.Mode == "auto" && err != nil) {
		if r.Cloud == nil {
			err = fmt.Errorf("cloud client unavailable")
		} else {
			snapshot, err = r.Cloud.Fetch(ctx)
			if err == nil {
				snapshot.Source = "cloud"
				err = r.accept(&snapshot)
			}
		}
	} else if r.Config.Mode == "auto" && r.Session != nil {
		// Keep the fallback cloud session renewed even while the local dish works.
		refresh, cancel := context.WithTimeout(ctx, r.Config.Timeout)
		_ = r.Session.EnsureSession(refresh)
		cancel()
	}
	if r.Session != nil {
		h := r.Session.Health()
		r.health.AuthRequired = h.AuthRequired
		r.health.CookieLastRefresh = h.CookieLastRefresh
		r.health.CookieExpiresAt = h.CookieExpiresAt
	}
	if err != nil {
		r.health.LastError = err.Error()
		r.health.PollErrors++
		r.Store.Update(nil, r.health)
	} else {
		now := time.Now().UTC()
		if snapshot.CollectedAt.IsZero() {
			snapshot.CollectedAt = now
		}
		r.health.LastSuccess = now
		r.health.LastError = ""
		r.health.Source = snapshot.Source
		r.Store.Update(&snapshot, r.health)
	}
	if r.Notify != nil {
		r.Notify()
	}
	return err
}

func (r *Runner) Run(ctx context.Context) {
	for {
		cycle, cancel := context.WithTimeout(ctx, 4*r.Config.Timeout+2*r.Config.LocalTimeout)
		err := r.Poll(cycle)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("collection failed", "error", err)
		} else {
			slog.Info("dish snapshot collected", "source", r.health.Source)
		}
		timer := time.NewTimer(r.Config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
