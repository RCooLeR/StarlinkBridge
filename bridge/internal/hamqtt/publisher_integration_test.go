package hamqtt

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Opt-in against a disposable broker, never the household broker in unit tests.
func TestBrokerLifecycle(t *testing.T) {
	broker := os.Getenv("STARLINK_TEST_MQTT_BROKER")
	if broker == "" {
		t.Skip("set STARLINK_TEST_MQTT_BROKER for disposable broker integration test")
	}
	config := cfg()
	config.MQTTBroker = broker
	config.MQTTTopic = "starlinkbridge_test"
	config.MQTTClientID = "starlinkbridge-test-publisher"
	config.DiscoveryPrefix = "starlinkbridge_test_discovery"
	updates := make(chan Message, 1000)
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID("starlinkbridge-test-observer")
	subscriber := mqtt.NewClient(opts)
	if token := subscriber.Connect(); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("connect observer: %v", token.Error())
	}
	defer subscriber.Disconnect(100)
	for _, topic := range []string{config.MQTTTopic + "/#", config.DiscoveryPrefix + "/#"} {
		token := subscriber.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			select {
			case updates <- Message{m.Topic(), append([]byte(nil), m.Payload()...)}:
			default:
			}
		})
		if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
			t.Fatal("subscribe failed")
		}
	}
	await := func(predicate func(Message) bool) Message {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case message := <-updates:
				if predicate(message) {
					return message
				}
			case <-timer.C:
				t.Fatal("timed out waiting for MQTT message")
				return Message{}
			}
		}
	}
	store := telemetry.NewStore(time.Minute)
	publisher := New(config, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { publisher.Run(ctx); close(done) }()
	await(func(m Message) bool {
		return m.Topic == config.MQTTTopic+"/data_availability" && string(m.Payload) == "offline"
	})
	for _, version := range []string{"version-A", "version-B", "version-A"} {
		s := fixture("cloud").Snapshot
		s.SoftwareVersion = version
		s.CollectedAt = time.Now()
		store.Update(s, model.Health{})
		publisher.Notify()
		await(func(m Message) bool {
			if !strings.HasSuffix(m.Topic, "/power_watts/config") {
				return false
			}
			var v map[string]any
			_ = json.Unmarshal(m.Payload, &v)
			device, _ := v["device"].(map[string]any)
			return device["sw_version"] == version
		})
		await(func(m Message) bool {
			return m.Topic == config.MQTTTopic+"/data_availability" && string(m.Payload) == "online"
		})
	}
	// HA birth must re-send discovery even with unchanged payloads.
	if token := subscriber.Publish(config.DiscoveryPrefix+"/status", 1, false, "online"); !token.WaitTimeout(3 * time.Second) {
		t.Fatal("birth publish timeout")
	}
	await(func(m Message) bool { return strings.HasSuffix(m.Topic, "/power_watts/config") })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not stop")
	}
	await(func(m Message) bool { return m.Topic == config.MQTTTopic+"/status" && string(m.Payload) == "offline" })
}
