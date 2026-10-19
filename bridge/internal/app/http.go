package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func Handler(store *telemetry.Store) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(telemetry.NewCollector(store))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !store.View(time.Now()).Fresh {
			http.Error(w, "no fresh dish data", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.View(time.Now()))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": "StarlinkBridge", "metrics": "/metrics", "status": "/api/v1/status", "health": "/healthz", "ready": "/readyz"})
	})
	return mux
}
