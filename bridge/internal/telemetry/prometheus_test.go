package telemetry

import (
	"testing"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsOptionalAndOutageGauges(t *testing.T) {
	store := NewStore(time.Minute)
	s := model.Snapshot{DeviceID: "ut123", Source: "cloud", CollectedAt: time.Now(), Values: map[string]float64{"power_watts": 32.7, "heating": 0}, HistoryAvailable: true, Outages: []model.Outage{{Cause: "NO_DOWNLINK", DurationSeconds: .44}}}
	store.Update(&s, model.Health{})
	r := prometheus.NewRegistry()
	r.MustRegister(NewCollector(store))
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
		if f.GetName() == "starlink_outages_window" && f.GetType().String() != "GAUGE" {
			t.Fatal("window count became a counter")
		}
	}
	if !names["starlink_power_watts"] || !names["starlink_heating"] || names["starlink_obstruction_ratio"] {
		t.Fatal(names)
	}
}
