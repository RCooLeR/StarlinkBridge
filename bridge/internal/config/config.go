package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Mode, LocalAddress, DeviceID, WebHost, CookieFile, CookieStateFile, Listen       string
	PollInterval, Timeout, LocalTimeout, StaleAfter, RefreshInterval                 time.Duration
	MQTTEnabled                                                                      bool
	MQTTBroker, MQTTUsername, MQTTPassword, MQTTClientID, MQTTTopic, DiscoveryPrefix string
}

func Load() (Config, error) { return FromEnv(os.LookupEnv) }

func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}
		return fallback
	}
	c := Config{
		Mode: get("STARLINK_MODE", "auto"), LocalAddress: get("STARLINK_LOCAL_ADDRESS", "192.168.100.1:9200"),
		DeviceID: get("STARLINK_DEVICE_ID", ""), WebHost: get("STARLINK_CLOUD_WEB_HOST", "starlink.com"),
		CookieFile:      get("STARLINK_COOKIE_FILE", "/app/config/starlink.cookie"),
		CookieStateFile: get("STARLINK_COOKIE_STATE_FILE", "/app/data/cookies.json"), Listen: get("STARLINK_LISTEN", ":9091"),
		MQTTBroker:   get("STARLINK_MQTT_BROKER", "tcp://mosquitto:1883"),
		MQTTUsername: get("STARLINK_MQTT_USERNAME", get("MQTT_USERNAME", "")),
		MQTTPassword: get("STARLINK_MQTT_PASSWORD", get("MQTT_PASSWORD", "")),
		MQTTClientID: get("STARLINK_MQTT_CLIENT_ID", "starlink-bridge"),
		MQTTTopic:    get("STARLINK_MQTT_TOPIC_PREFIX", "starlinkbridge"), DiscoveryPrefix: get("STARLINK_MQTT_DISCOVERY_PREFIX", "homeassistant"),
	}
	for _, item := range []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"STARLINK_POLL_INTERVAL", "60s", &c.PollInterval}, {"STARLINK_TIMEOUT", "20s", &c.Timeout},
		{"STARLINK_LOCAL_TIMEOUT", "5s", &c.LocalTimeout}, {"STARLINK_STALE_AFTER", "3m", &c.StaleAfter},
		{"STARLINK_COOKIE_REFRESH_INTERVAL", "30m", &c.RefreshInterval},
	} {
		value, err := time.ParseDuration(get(item.key, item.fallback))
		if err != nil || value <= 0 {
			return c, fmt.Errorf("%s must be a positive duration", item.key)
		}
		*item.target = value
	}
	var err error
	c.MQTTEnabled, err = strconv.ParseBool(get("STARLINK_MQTT_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("STARLINK_MQTT_ENABLED must be true or false")
	}
	if c.Mode != "local" && c.Mode != "cloud" && c.Mode != "auto" {
		return c, fmt.Errorf("STARLINK_MODE must be local, cloud, or auto")
	}
	if c.DeviceID != "" {
		if !strings.HasPrefix(c.DeviceID, "ut") {
			c.DeviceID = "ut" + c.DeviceID
		}
		if !regexp.MustCompile(`^ut[A-Za-z0-9-]{1,128}$`).MatchString(c.DeviceID) {
			return c, fmt.Errorf("invalid STARLINK_DEVICE_ID")
		}
	}
	if _, _, err := net.SplitHostPort(c.LocalAddress); err != nil {
		return c, fmt.Errorf("STARLINK_LOCAL_ADDRESS must be host:port")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return c, fmt.Errorf("STARLINK_LISTEN must be host:port")
	}
	if c.WebHost != "starlink.com" && c.WebHost != "www.starlink.com" {
		return c, fmt.Errorf("STARLINK_CLOUD_WEB_HOST must be starlink.com or www.starlink.com")
	}
	if c.PollInterval < 10*time.Second {
		return c, fmt.Errorf("STARLINK_POLL_INTERVAL must be at least 10s")
	}
	if c.StaleAfter < c.PollInterval+c.Timeout+c.LocalTimeout {
		return c, fmt.Errorf("STARLINK_STALE_AFTER must exceed poll interval plus both request timeouts")
	}
	if c.RefreshInterval < time.Minute || c.RefreshInterval > 12*time.Hour {
		return c, fmt.Errorf("STARLINK_COOKIE_REFRESH_INTERVAL must be between 1m and 12h")
	}
	if c.Mode != "local" && (c.CookieFile == "" || c.CookieStateFile == "" || c.CookieFile == c.CookieStateFile) {
		return c, fmt.Errorf("cloud mode requires separate cookie import and state file paths")
	}
	if c.MQTTEnabled {
		u, err := url.Parse(c.MQTTBroker)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "tcp" && u.Scheme != "ssl" && u.Scheme != "tls" && u.Scheme != "ws" && u.Scheme != "wss") {
			return c, fmt.Errorf("STARLINK_MQTT_BROKER must be an MQTT URL without embedded credentials")
		}
		for _, topic := range []string{c.MQTTTopic, c.DiscoveryPrefix} {
			if topic == "" || strings.ContainsAny(topic, "+#\x00") || strings.HasSuffix(topic, "/") {
				return c, fmt.Errorf("MQTT topic prefixes must be nonempty without wildcards or trailing slash")
			}
		}
		if c.MQTTClientID == "" {
			return c, fmt.Errorf("STARLINK_MQTT_CLIENT_ID must not be empty")
		}
	}
	return c, nil
}
