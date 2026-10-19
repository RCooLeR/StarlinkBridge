package telemetry

type Definition struct {
	Key, Name, Help, Unit, DeviceClass, StateClass, Icon string
	Scale                                                float64
	Binary                                               bool
}

// Keys and Prometheus values use base units. Unit/Scale describe HA presentation.
var Definitions = []Definition{
	{Key: "power_watts", Name: "Power", Help: "Latest power sample in the returned dish history, watts.", Unit: "W", DeviceClass: "power", StateClass: "measurement", Scale: 1},
	{Key: "power_mean_watts", Name: "Power average", Help: "Mean power over the returned history window, watts.", Unit: "W", DeviceClass: "power", StateClass: "measurement", Scale: 1},
	{Key: "power_min_watts", Name: "Power minimum", Help: "Minimum power over the returned history window, watts.", Unit: "W", DeviceClass: "power", StateClass: "measurement", Scale: 1},
	{Key: "power_max_watts", Name: "Power maximum", Help: "Maximum power over the returned history window, watts.", Unit: "W", DeviceClass: "power", StateClass: "measurement", Scale: 1},
	{Key: "power_samples", Name: "Power samples", Help: "Finite samples in the returned power history.", Scale: 1},
	{Key: "downlink_bps", Name: "Download throughput", Help: "Dish downlink throughput in bits per second, not a speed test.", Unit: "Mbit/s", DeviceClass: "data_rate", StateClass: "measurement", Scale: 1e-6},
	{Key: "uplink_bps", Name: "Upload throughput", Help: "Dish uplink throughput in bits per second, not a speed test.", Unit: "Mbit/s", DeviceClass: "data_rate", StateClass: "measurement", Scale: 1e-6},
	{Key: "latency_seconds", Name: "Latency", Help: "Dish to Starlink point-of-presence ping latency, seconds.", Unit: "ms", DeviceClass: "duration", StateClass: "measurement", Scale: 1000},
	{Key: "ping_drop_ratio", Name: "Ping loss", Help: "Dish ping loss ratio.", Unit: "%", StateClass: "measurement", Scale: 100},
	{Key: "obstruction_ratio", Name: "Obstruction", Help: "Dish reported obstruction fraction.", Unit: "%", StateClass: "measurement", Scale: 100},
	{Key: "uptime_seconds", Name: "Uptime", Help: "Dish uptime since boot, seconds.", Unit: "s", DeviceClass: "duration", StateClass: "measurement", Scale: 1},
	{Key: "current_outage", Name: "Outage", Help: "Whether a current dish outage was reported.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "current_outage_duration_seconds", Name: "Current outage duration", Help: "Reported duration of the current outage, seconds.", Unit: "s", DeviceClass: "duration", StateClass: "measurement", Scale: 1},
	{Key: "history_outages", Name: "Outages in history", Help: "Outage records in the returned history window, not a lifetime counter.", Scale: 1},
	{Key: "last_outage_duration_seconds", Name: "Last outage duration", Help: "Duration of the newest outage in the returned history, seconds.", Unit: "s", DeviceClass: "duration", Scale: 1},
	{Key: "history_outage_seconds", Name: "Outage time in history", Help: "Sum of outage durations in the returned history window, seconds.", Unit: "s", DeviceClass: "duration", Scale: 1},
	{Key: "history_samples", Name: "History samples", Help: "Populated samples in the returned shared history buffer.", Scale: 1},
	{Key: "heating", Name: "Heating", Help: "Dish heating alert.", Binary: true, DeviceClass: "heat", Scale: 1},
	{Key: "thermal_throttle", Name: "Thermal throttling", Help: "Dish thermal throttling alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "thermal_shutdown", Name: "Thermal shutdown", Help: "Dish thermal shutdown alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "power_supply_thermal_throttle", Name: "Power supply throttling", Help: "Power supply thermal throttling alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "motors_stuck", Name: "Motors stuck", Help: "Dish motors stuck alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "mast_not_near_vertical", Name: "Mast alignment problem", Help: "Dish mast not near vertical alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "slow_ethernet", Name: "Slow Ethernet", Help: "Dish slow Ethernet alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "unexpected_location", Name: "Unexpected location", Help: "Dish unexpected location alert.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "signal_above_noise_floor", Name: "Signal above noise floor", Help: "Dish signal above noise floor flag.", Binary: true, Scale: 1},
	{Key: "signal_persistently_low", Name: "Low signal", Help: "Dish persistent low signal flag.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "currently_obstructed", Name: "Currently obstructed", Help: "Dish currently obstructed flag.", Binary: true, DeviceClass: "problem", Scale: 1},
	{Key: "obstruction_valid_seconds", Name: "Obstruction observation time", Help: "Dish obstruction valid_s field, seconds.", Unit: "s", DeviceClass: "duration", Scale: 1},
	{Key: "azimuth_degrees", Name: "Azimuth", Help: "Physical dish boresight azimuth, degrees.", Unit: "°", StateClass: "measurement", Scale: 1},
	{Key: "elevation_degrees", Name: "Elevation", Help: "Physical dish boresight elevation, degrees.", Unit: "°", StateClass: "measurement", Scale: 1},
	{Key: "tilt_degrees", Name: "Tilt", Help: "Physical dish alignment tilt, degrees.", Unit: "°", StateClass: "measurement", Scale: 1},
	{Key: "desired_azimuth_degrees", Name: "Desired azimuth", Help: "Dish requested alignment azimuth, degrees.", Unit: "°", Scale: 1},
	{Key: "desired_elevation_degrees", Name: "Desired elevation", Help: "Dish requested alignment elevation, degrees.", Unit: "°", Scale: 1},
	{Key: "update_reboot_required", Name: "Update reboot required", Help: "Dish reports an update requiring reboot.", Binary: true, DeviceClass: "update", Scale: 1},
}
