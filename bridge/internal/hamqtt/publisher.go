package hamqtt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/config"
	"github.com/RCooLeR/StarlinkBridge/internal/telemetry"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Message struct {
	Topic   string
	Payload []byte
}

type Publisher struct {
	config config.Config
	store  *telemetry.Store
	client mqtt.Client
	mu     sync.Mutex
	known  map[string]string
	wake   chan struct{}
}

func New(cfg config.Config, store *telemetry.Store) *Publisher {
	return &Publisher{config: cfg, store: store, known: map[string]string{}, wake: make(chan struct{}, 1)}
}
func (p *Publisher) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func deviceSlug(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// Messages is pure and testable. Every optional measurement has an independent
// availability topic so a disappearing field never leaves a stale HA reading.
func Messages(cfg config.Config, view telemetry.View) []Message {
	var out []Message
	add := func(topic string, value any) { data, _ := json.Marshal(value); out = append(out, Message{topic, data}) }
	plain := func(topic, value string) { out = append(out, Message{topic, []byte(value)}) }
	base := cfg.MQTTTopic
	if !view.Fresh || view.Snapshot == nil {
		plain(base+"/data_availability", "offline")
	}
	bridgeDevice := map[string]any{"identifiers": []string{base + "_bridge"}, "name": "Starlink Bridge", "manufacturer": "StarlinkBridge", "model": "Local / cloud telemetry bridge"}
	add(cfg.DiscoveryPrefix+"/binary_sensor/"+cfg.MQTTClientID+"/auth_required/config", map[string]any{
		"name": "Cloud authentication required", "unique_id": base + "_auth_required", "state_topic": base + "/auth_required",
		"payload_on": "ON", "payload_off": "OFF", "device_class": "problem", "entity_category": "diagnostic",
		"availability_topic": base + "/status", "device": bridgeDevice,
	})
	if view.Health.AuthRequired {
		plain(base+"/auth_required", "ON")
	} else {
		plain(base+"/auth_required", "OFF")
	}
	if view.Snapshot == nil {
		return out
	}
	s := view.Snapshot
	slug := deviceSlug(s.DeviceID)
	topic := base + "/" + slug
	device := map[string]any{"identifiers": []string{"starlink_" + s.DeviceID}, "name": "Starlink dish", "manufacturer": "SpaceX", "model": s.HardwareVersion, "sw_version": s.SoftwareVersion, "via_device": base + "_bridge"}
	state := map[string]any{"source": s.Source, "software_update_state": s.States["software_update_state"], "current_outage_cause": s.States["current_outage_cause"], "last_outage_cause": s.States["last_outage_cause"]}
	for _, def := range telemetry.Definitions {
		component := "sensor"
		if def.Binary {
			component = "binary_sensor"
		}
		entity := map[string]any{
			"name": def.Name, "unique_id": "starlink_" + slug + "_" + def.Key, "state_topic": topic + "/state",
			"value_template": "{{ value_json." + def.Key + " | default('None') }}", "device": device,
			"availability_mode": "all", "availability": []map[string]string{{"topic": base + "/status"}, {"topic": base + "/data_availability"}, {"topic": topic + "/availability"}, {"topic": topic + "/" + def.Key + "/availability"}},
			"expire_after": int(cfg.StaleAfter.Seconds()),
		}
		if def.Unit != "" {
			entity["unit_of_measurement"] = def.Unit
		}
		if def.DeviceClass != "" {
			entity["device_class"] = def.DeviceClass
		}
		if def.StateClass != "" {
			entity["state_class"] = def.StateClass
		}
		if def.Binary {
			entity["payload_on"] = "ON"
			entity["payload_off"] = "OFF"
		}
		if def.Key != "power_watts" && def.Key != "current_outage" {
			entity["entity_category"] = "diagnostic"
		}
		add(cfg.DiscoveryPrefix+"/"+component+"/starlink_"+slug+"/"+def.Key+"/config", entity)
		value, ok := s.Values[def.Key]
		available := view.Fresh && ok && !math.IsNaN(value) && !math.IsInf(value, 0)
		if available {
			plain(topic+"/"+def.Key+"/availability", "online")
			if def.Binary {
				if value != 0 {
					state[def.Key] = "ON"
				} else {
					state[def.Key] = "OFF"
				}
			} else {
				state[def.Key] = value * def.Scale
			}
		} else {
			plain(topic+"/"+def.Key+"/availability", "offline")
		}
	}
	for key, name := range map[string]string{"source": "Data source", "software_update_state": "Software update", "current_outage_cause": "Outage cause", "last_outage_cause": "Last outage cause"} {
		value, _ := state[key].(string)
		add(cfg.DiscoveryPrefix+"/sensor/starlink_"+slug+"/"+key+"/config", map[string]any{
			"name": name, "unique_id": "starlink_" + slug + "_" + key, "device": device, "state_topic": topic + "/state",
			"value_template": "{{ value_json." + key + " | default('None') }}", "entity_category": "diagnostic",
			"availability_mode": "all", "availability": []map[string]string{{"topic": base + "/status"}, {"topic": base + "/data_availability"}, {"topic": topic + "/availability"}, {"topic": topic + "/" + key + "/availability"}},
			"expire_after": int(cfg.StaleAfter.Seconds()),
		})
		if view.Fresh && value != "" {
			plain(topic+"/"+key+"/availability", "online")
		} else {
			plain(topic+"/"+key+"/availability", "offline")
		}
	}
	if view.Fresh {
		add(topic+"/state", state)
		add(topic+"/outages", s.Outages)
		plain(topic+"/availability", "online")
		plain(base+"/data_availability", "online")
	} else {
		plain(topic+"/availability", "offline")
	}
	return out
}

func wait(ctx context.Context, t mqtt.Token) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.Done():
		return t.Error()
	}
}

func (p *Publisher) publish(ctx context.Context) error {
	ctx, cancelBatch := context.WithTimeout(ctx, 20*time.Second)
	defer cancelBatch()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.client.IsConnectionOpen() {
		return fmt.Errorf("MQTT disconnected")
	}
	for _, message := range Messages(p.config, p.store.View(time.Now())) {
		// Only discovery is deduplicated; states are refreshed to drive expire_after.
		if strings.HasSuffix(message.Topic, "/config") {
			if previous, ok := p.known[message.Topic]; ok && previous == string(message.Payload) {
				continue
			}
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := wait(call, p.client.Publish(message.Topic, 1, true, message.Payload))
		cancel()
		if err != nil {
			return fmt.Errorf("MQTT publish failed")
		}
		if strings.HasSuffix(message.Topic, "/config") {
			p.known[message.Topic] = string(message.Payload)
		}
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wait(call, p.client.Publish(p.config.MQTTTopic+"/status", 1, true, "online"))
}

func (p *Publisher) Run(ctx context.Context) {
	opts := mqtt.NewClientOptions().AddBroker(p.config.MQTTBroker).SetClientID(p.config.MQTTClientID)
	opts.SetUsername(p.config.MQTTUsername).SetPassword(p.config.MQTTPassword)
	opts.SetConnectTimeout(10 * time.Second).SetWriteTimeout(5 * time.Second).SetAutoReconnect(true).SetMaxReconnectInterval(time.Minute)
	opts.SetCleanSession(true).SetOrderMatters(false).SetWill(p.config.MQTTTopic+"/status", "offline", 1, true)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		p.mu.Lock()
		p.known = map[string]string{}
		p.mu.Unlock()
		client.Subscribe(p.config.DiscoveryPrefix+"/status", 1, func(_ mqtt.Client, m mqtt.Message) {
			if string(m.Payload()) == "online" {
				p.mu.Lock()
				p.known = map[string]string{}
				p.mu.Unlock()
				p.Notify()
			}
		})
		p.Notify()
	})
	p.client = mqtt.NewClient(opts)
	defer func() {
		if p.client.IsConnectionOpen() {
			token := p.client.Publish(p.config.MQTTTopic+"/status", 1, true, "offline")
			token.WaitTimeout(2 * time.Second)
		}
		p.client.Disconnect(250)
	}()
	// Manual initial retries keep HTTP/collection available while the broker is down.
	for !p.client.IsConnectionOpen() {
		call, cancel := context.WithTimeout(ctx, 12*time.Second)
		err := wait(call, p.client.Connect())
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("MQTT connection unavailable; retrying", "retry_seconds", 30)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wake:
		}
		if err := p.publish(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("MQTT update failed; retrying on next tick")
		}
	}
}
