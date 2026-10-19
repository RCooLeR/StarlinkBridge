package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var devicePattern = regexp.MustCompile(`^ut[A-Za-z0-9-]{1,128}$`)

// Local device_info.id may omit the ut prefix required by the cloud API.
func NormalizeDeviceID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("dish response has no device ID")
	}
	if !strings.HasPrefix(id, "ut") {
		id = "ut" + id
	}
	if !devicePattern.MatchString(id) {
		return "", fmt.Errorf("invalid Starlink device ID")
	}
	return id, nil
}

// Snapshot is a single read of a dish. Omitted values are unavailable, not zero.
// Measurements use Prometheus base units; presentation conversions live in MQTT.
type Snapshot struct {
	DeviceID         string             `json:"device_id"`
	HardwareVersion  string             `json:"hardware_version,omitempty"`
	SoftwareVersion  string             `json:"software_version,omitempty"`
	Source           string             `json:"source"`
	CollectedAt      time.Time          `json:"collected_at"`
	Values           map[string]float64 `json:"values"`
	States           map[string]string  `json:"states"`
	Outages          []Outage           `json:"outages,omitempty"`
	HistoryAvailable bool               `json:"history_available"`
	HistoryError     string             `json:"history_error,omitempty"`
}

type Outage struct {
	Cause           string  `json:"cause"`
	StartNS         int64   `json:"start_ns"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// Health describes collector/session state without exposing credentials or HTTP bodies.
type Health struct {
	LastAttempt       time.Time `json:"last_attempt,omitempty"`
	LastSuccess       time.Time `json:"last_success,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	Source            string    `json:"source,omitempty"`
	AuthRequired      bool      `json:"auth_required"`
	CookieLastRefresh time.Time `json:"cookie_last_refresh,omitempty"`
	CookieExpiresAt   time.Time `json:"cookie_expires_at,omitempty"`
	PollErrors        uint64    `json:"poll_errors"`
}

// Value keys shared by protocol, Prometheus, and MQTT. Unknown values are omitted.
// downlink_bps, uplink_bps, latency_seconds, ping_drop_ratio, obstruction_ratio,
// uptime_seconds, power_watts, power_mean_watts, power_min_watts, power_max_watts,
// power_samples, current_outage, current_outage_duration_seconds,
// heating, thermal_throttle, thermal_shutdown, power_supply_thermal_throttle,
// motors_stuck, mast_not_near_vertical, slow_ethernet, unexpected_location,
// signal_above_noise_floor, signal_persistently_low, currently_obstructed,
// obstruction_valid_seconds, azimuth_degrees, elevation_degrees, tilt_degrees,
// desired_azimuth_degrees, desired_elevation_degrees, update_reboot_required.
// history_outages, history_outage_seconds, history_samples, last_outage_duration_seconds.
// State keys: current_outage_cause, last_outage_cause, software_update_state.
