package config

import "testing"

func TestConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		bad  bool
	}{
		{"default", nil, false},
		{"cloud", map[string]string{"STARLINK_MODE": "cloud", "STARLINK_MQTT_ENABLED": "true"}, false},
		{"unknownmode", map[string]string{"STARLINK_MODE": "both"}, true},
		{"fastpoll", map[string]string{"STARLINK_POLL_INTERVAL": "1s"}, true},
		{"expirytooearly", map[string]string{"STARLINK_STALE_AFTER": "10s"}, true},
		{"badcloudhost", map[string]string{"STARLINK_CLOUD_WEB_HOST": "example.com"}, true},
		{"samecookies", map[string]string{"STARLINK_COOKIE_FILE": "x", "STARLINK_COOKIE_STATE_FILE": "x"}, true},
		{"topicwildcard", map[string]string{"STARLINK_MQTT_ENABLED": "true", "STARLINK_MQTT_TOPIC_PREFIX": "bad/#"}, true},
		{"brokerpasswordurl", map[string]string{"STARLINK_MQTT_ENABLED": "true", "STARLINK_MQTT_BROKER": "tcp://user:secret@host:1883"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := FromEnv(func(k string) (string, bool) { v, ok := test.env[k]; return v, ok })
			if (err != nil) != test.bad {
				t.Fatalf("error=%v expectedbad=%v", err, test.bad)
			}
		})
	}
}
