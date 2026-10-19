package hamqtt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/config"
	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
)

func fixture(source string) telemetry.View {
	return telemetry.View{Fresh: true, Snapshot: &model.Snapshot{DeviceID: "ut123", Source: source, Values: map[string]float64{"power_watts": 32.7, "latency_seconds": .02, "heating": 0}, States: map[string]string{}}}
}
func cfg() config.Config {
	return config.Config{MQTTTopic: "starlinkbridge", MQTTClientID: "starlink-bridge", DiscoveryPrefix: "homeassistant", StaleAfter: 3 * time.Minute}
}
func lookup(messages []Message, suffix string) []byte {
	for _, m := range messages {
		if strings.HasSuffix(m.Topic, suffix) {
			return m.Payload
		}
	}
	return nil
}

func TestDiscoveryStableAcrossSourcesAndUnits(t *testing.T) {
	local := Messages(cfg(), fixture("local"))
	cloud := Messages(cfg(), fixture("cloud"))
	for _, suffix := range []string{"/power_watts/config", "/latency_seconds/config", "/heating/config"} {
		if string(lookup(local, suffix)) != string(lookup(cloud, suffix)) {
			t.Fatal("discovery IDs changed with source")
		}
	}
	var entity map[string]any
	if err := json.Unmarshal(lookup(local, "/power_watts/config"), &entity); err != nil {
		t.Fatal(err)
	}
	if entity["device_class"] != "power" || entity["unit_of_measurement"] != "W" || entity["state_class"] != "measurement" {
		t.Fatal(entity)
	}
	var state map[string]any
	if err := json.Unmarshal(lookup(local, "/state"), &state); err != nil {
		t.Fatal(err)
	}
	if state["latency_seconds"] != float64(20) || state["heating"] != "OFF" {
		t.Fatal(state)
	}
}

func TestMissingAndStaleValuesUnavailable(t *testing.T) {
	view := fixture("cloud")
	delete(view.Snapshot.Values, "power_watts")
	messages := Messages(cfg(), view)
	if string(lookup(messages, "/power_watts/availability")) != "offline" {
		t.Fatal("missing power is available")
	}
	if strings.Contains(string(lookup(messages, "/state")), "power_watts") {
		t.Fatal("invented power value")
	}
	view.Fresh = false
	messages = Messages(cfg(), view)
	if lookup(messages, "/state") != nil {
		t.Fatal("republished stale state")
	}
	for _, m := range messages {
		if strings.HasSuffix(m.Topic, "/availability") && string(m.Payload) != "offline" {
			t.Fatal("stale availability online")
		}
	}
}

func TestStartupDisablesRetainedDishData(t *testing.T) {
	messages := Messages(cfg(), telemetry.View{})
	if string(lookup(messages, "/data_availability")) != "offline" {
		t.Fatal("startup left retained data enabled")
	}
	var entity map[string]any
	_ = json.Unmarshal(lookup(Messages(cfg(), fixture("cloud")), "/power_watts/config"), &entity)
	if !strings.Contains(string(lookup(Messages(cfg(), fixture("cloud")), "/power_watts/config")), "starlinkbridge/data_availability") {
		t.Fatal("dish entity lacks shared freshness gate")
	}
}
