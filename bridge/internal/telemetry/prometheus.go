package telemetry

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Collector struct {
	store       *Store
	descriptors map[string]*prometheus.Desc
}

func NewCollector(store *Store) *Collector {
	c := &Collector{store: store, descriptors: map[string]*prometheus.Desc{}}
	for _, d := range Definitions {
		c.descriptors[d.Key] = prometheus.NewDesc("starlink_"+d.Key, d.Help, []string{"device_id"}, nil)
	}
	return c
}
func (c *Collector) Describe(ch chan<- *prometheus.Desc) { /* unchecked collector with a fixed catalog */
}
func gauge(ch chan<- prometheus.Metric, name, help string, value float64) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_bridge_"+name, help, nil, nil), prometheus.GaugeValue, value)
}
func truth(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	v := c.store.View(now)
	gauge(ch, "data_fresh", "Whether the last successful snapshot is within the configured freshness limit.", truth(v.Fresh))
	gauge(ch, "auth_required", "Whether the cloud session needs a new cookie or is access-blocked.", truth(v.Health.AuthRequired))
	gauge(ch, "last_poll_success", "Whether the last collection cycle completed successfully.", truth(!v.Health.LastSuccess.IsZero() && v.Health.LastError == ""))
	for name, stamp := range map[string]time.Time{"last_success_timestamp_seconds": v.Health.LastSuccess, "last_attempt_timestamp_seconds": v.Health.LastAttempt, "cookie_refresh_timestamp_seconds": v.Health.CookieLastRefresh, "cookie_expiry_timestamp_seconds": v.Health.CookieExpiresAt} {
		if !stamp.IsZero() {
			gauge(ch, name, "Unix timestamp for the named bridge lifecycle event.", float64(stamp.Unix()))
		}
	}
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_bridge_poll_errors_total", "Failed collection cycles since bridge start.", nil, nil), prometheus.CounterValue, float64(v.Health.PollErrors))
	if v.Snapshot == nil {
		return
	}
	s := v.Snapshot
	gauge(ch, "snapshot_age_seconds", "Age of the last successful collection, not dish sample age.", math.Max(0, now.Sub(s.CollectedAt).Seconds()))
	if !v.Fresh {
		return
	}
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_device_info", "Dish identity and current collection source.", []string{"device_id", "hardware_version", "software_version", "source"}, nil), prometheus.GaugeValue, 1, s.DeviceID, s.HardwareVersion, s.SoftwareVersion, s.Source)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_history_available", "Whether history was retrieved for this snapshot.", []string{"device_id"}, nil), prometheus.GaugeValue, truth(s.HistoryAvailable), s.DeviceID)
	for _, key := range []string{"current_outage_cause", "last_outage_cause", "software_update_state"} {
		if value := s.States[key]; value != "" {
			ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_"+key+"_info", "Dish reported state for the named diagnostic.", []string{"device_id", "state"}, nil), prometheus.GaugeValue, 1, s.DeviceID, value)
		}
	}
	for key, value := range s.Values {
		if desc, ok := c.descriptors[key]; ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, s.DeviceID)
		}
	}
	if s.HistoryAvailable {
		counts, durations := map[string]float64{}, map[string]float64{}
		for _, o := range s.Outages {
			counts[o.Cause]++
			durations[o.Cause] += o.DurationSeconds
		}
		for cause, count := range counts {
			ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_outages_window", "Outages by cause in the returned history window, not a lifetime counter.", []string{"device_id", "cause"}, nil), prometheus.GaugeValue, count, s.DeviceID, cause)
			ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("starlink_outage_duration_window_seconds", "Outage duration by cause in the returned history window.", []string{"device_id", "cause"}, nil), prometheus.GaugeValue, durations[cause], s.DeviceID, cause)
		}
	}
}
