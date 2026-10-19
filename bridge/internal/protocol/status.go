package protocol

import (
	"errors"
	"fmt"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

var outageCauses = map[uint64]string{0: "UNKNOWN", 1: "BOOTING", 2: "STOWED", 3: "THERMAL_SHUTDOWN", 4: "NO_SCHEDULE", 5: "NO_SATS", 6: "OBSTRUCTED", 7: "NO_DOWNLINK", 8: "NO_PINGS", 9: "ACTUATOR_ACTIVITY", 10: "CABLE_TEST", 11: "SLEEPING", 13: "SKY_SEARCH", 14: "INHIBIT_RF"}
var updateStates = map[uint64]string{0: "UNKNOWN", 1: "IDLE", 2: "FETCHING", 3: "PRE_CHECK", 4: "WRITING", 5: "POST_CHECK", 6: "REBOOT_REQUIRED", 7: "DISABLED", 8: "FAULTED"}

func enumName(value uint64, names map[uint64]string) string {
	if name, ok := names[value]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN_CODE_%d", value)
}

type valueReader struct {
	values map[string]float64
	err    error
}

func (r *valueReader) float(m message, num protowire.Number, key string, scale float64, zeroDefault bool) {
	if r.err != nil {
		return
	}
	x, ok, err := m.float(num)
	r.err = err
	if err == nil && (ok || (zeroDefault && len(m[num]) == 0)) {
		r.values[key] = x * scale
	}
}

func (r *valueReader) uint(m message, num protowire.Number, key string, boolValue, zeroDefault bool) {
	if r.err != nil {
		return
	}
	x, ok, err := m.uint(num)
	r.err = err
	if err == nil && (ok || zeroDefault) {
		if boolValue && x != 0 {
			x = 1
		}
		r.values[key] = float64(x)
	}
}

// DecodeStatus converts a Device.Response into measurements in base units.
// Unsupported optional parent messages remain absent; supported proto3 defaults
// are zero, notably alert flags, so a cleared alert replaces a previous true.
func DecodeStatus(data []byte) (model.Snapshot, error) {
	s := model.Snapshot{Values: make(map[string]float64), States: make(map[string]string)}
	dish, err := response(data, 2004)
	if err != nil {
		return s, err
	}
	if len(dish) == 0 {
		return s, errors.New("empty dish status")
	}
	info, _, err := dish.nested(1)
	if err != nil {
		return s, err
	}
	if s.DeviceID, err = info.text(1); err != nil {
		return s, err
	}
	if s.HardwareVersion, err = info.text(2); err != nil {
		return s, err
	}
	if s.SoftwareVersion, err = info.text(3); err != nil {
		return s, err
	}
	r := valueReader{values: s.Values}
	r.float(dish, 1007, "downlink_bps", 1, true)
	r.float(dish, 1008, "uplink_bps", 1, true)
	r.float(dish, 1009, "latency_seconds", 0.001, true)
	r.float(dish, 1003, "ping_drop_ratio", 1, true)
	state, ok, err := dish.nested(2)
	if err != nil {
		return s, err
	}
	if ok {
		r.uint(state, 1, "uptime_seconds", false, true)
	}
	alerts, ok, err := dish.nested(1005)
	if err != nil {
		return s, err
	}
	if ok {
		for num, key := range map[protowire.Number]string{1: "motors_stuck", 2: "thermal_shutdown", 3: "thermal_throttle", 4: "unexpected_location", 5: "mast_not_near_vertical", 6: "slow_ethernet", 9: "heating", 10: "power_supply_thermal_throttle"} {
			r.uint(alerts, num, key, true, true)
		}
		// Either Ethernet warning is useful; the 100 Mbps flag appeared later.
		slow100, _, e := alerts.uint(18)
		if e != nil {
			return s, e
		}
		if slow100 != 0 {
			s.Values["slow_ethernet"] = 1
		}
	}
	obstruction, ok, err := dish.nested(1004)
	if err != nil {
		return s, err
	}
	if ok {
		r.float(obstruction, 1, "obstruction_ratio", 1, true)
		r.uint(obstruction, 5, "currently_obstructed", true, true)
		r.float(obstruction, 4, "obstruction_valid_seconds", 1, false)
	}
	r.uint(dish, 1018, "signal_above_noise_floor", true, false)
	r.uint(dish, 1022, "signal_persistently_low", true, false)
	r.float(dish, 1011, "azimuth_degrees", 1, false)
	r.float(dish, 1012, "elevation_degrees", 1, false)
	alignment, ok, err := dish.nested(1027)
	if err != nil {
		return s, err
	}
	if ok {
		r.float(alignment, 3, "tilt_degrees", 1, false)
		r.float(alignment, 4, "azimuth_degrees", 1, false)
		r.float(alignment, 5, "elevation_degrees", 1, false)
		r.float(alignment, 8, "desired_azimuth_degrees", 1, false)
		r.float(alignment, 9, "desired_elevation_degrees", 1, false)
	}
	update, updateOK, err := dish.nested(1026)
	if err != nil {
		return s, err
	}
	if updateOK {
		r.uint(update, 3, "update_reboot_required", true, true)
	}
	updateCode, updateCodeOK, err := dish.uint(1021)
	if err != nil {
		return s, err
	}
	if !updateCodeOK && updateOK {
		updateCode, _, err = update.uint(1)
		if err != nil {
			return s, err
		}
		updateCodeOK = true
	}
	if updateCodeOK {
		s.States["software_update_state"] = enumName(updateCode, updateStates)
	}
	outage, ok, err := dish.nested(1014)
	if err != nil {
		return s, err
	}
	s.Values["current_outage"] = 0
	s.Values["current_outage_duration_seconds"] = 0
	s.States["current_outage_cause"] = "NONE"
	if ok {
		o, e := decodeOutage(outage)
		if e != nil {
			return s, e
		}
		s.Values["current_outage"] = 1
		s.Values["current_outage_duration_seconds"] = o.DurationSeconds
		s.States["current_outage_cause"] = o.Cause
	}
	if r.err != nil {
		return s, r.err
	}
	return s, nil
}

func decodeOutage(m message) (model.Outage, error) {
	var o model.Outage
	cause, _, err := m.uint(1)
	if err != nil {
		return o, err
	}
	o.Cause = enumName(cause, outageCauses)
	start, _, err := m.uint(2)
	if err != nil {
		return o, err
	}
	o.StartNS = int64(start)
	duration, _, err := m.uint(3)
	if err != nil {
		return o, err
	}
	o.DurationSeconds = float64(duration) / 1e9
	return o, nil
}
