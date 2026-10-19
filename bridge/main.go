package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/app"
	"github.com/RCooLeR/StarlinkBridge/internal/cloud"
	"github.com/RCooLeR/StarlinkBridge/internal/config"
	"github.com/RCooLeR/StarlinkBridge/internal/hamqtt"
	"github.com/RCooLeR/StarlinkBridge/internal/local"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
)

var version = "dev"

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "help" || command == "--help" || command == "-h" {
		fmt.Println("StarlinkBridge: serve | fetch | healthcheck | version. Configure with STARLINK_* environment variables; see README.md.")
		return nil
	}
	if command == "healthcheck" {
		address := os.Getenv("STARLINK_HEALTHCHECK_URL")
		if address == "" {
			address = "http://127.0.0.1:9091/healthz"
		}
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(address)
		if err != nil {
			return fmt.Errorf("healthcheck failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthcheck failed")
		}
		return nil
	}
	if command != "serve" && command != "fetch" {
		return fmt.Errorf("unknown command; use serve, fetch, healthcheck, or version")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var workers sync.WaitGroup
	defer func() {
		stop()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
		}
	}()
	store := telemetry.NewStore(cfg.StaleAfter)
	runner := &app.Runner{Config: cfg, Store: store}
	if cfg.Mode != "cloud" {
		client, err := local.New(cfg.LocalAddress, cfg.LocalTimeout)
		if err != nil {
			return err
		}
		defer client.Close()
		runner.Local = client
	}
	if cfg.Mode != "local" {
		client, err := cloud.New(cloud.Config{CookieFile: cfg.CookieFile, StateFile: cfg.CookieStateFile, WebHost: cfg.WebHost, DeviceID: cfg.DeviceID, Timeout: cfg.Timeout, RefreshInterval: cfg.RefreshInterval})
		if err != nil {
			return err
		}
		runner.Cloud = client
		runner.Session = client
	}
	if command == "fetch" {
		call, cancel := context.WithTimeout(ctx, 4*cfg.Timeout+2*cfg.LocalTimeout)
		defer cancel()
		if err := runner.Poll(call); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(store.View(time.Now()))
	}
	if cfg.MQTTEnabled {
		publisher := hamqtt.New(cfg, store)
		runner.Notify = publisher.Notify
		workers.Add(1)
		go func() { defer workers.Done(); publisher.Run(ctx) }()
	}
	workers.Add(1)
	go func() { defer workers.Done(); runner.Run(ctx) }()
	server := &http.Server{Addr: cfg.Listen, Handler: app.Handler(store), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	slog.Info("StarlinkBridge started", "version", version, "mode", cfg.Mode, "listen", cfg.Listen)
	select {
	case <-ctx.Done():
	case err := <-serverError:
		if err != nil && err != http.ErrServerClosed {
			stop()
			return err
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("StarlinkBridge stopped", "error", err)
		os.Exit(1)
	}
}
