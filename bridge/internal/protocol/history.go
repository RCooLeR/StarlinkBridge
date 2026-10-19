package protocol

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

func repeatedFloats(m message, num protowire.Number) ([]float64, error) {
	var values []float64
	for _, f := range m[num] {
		switch f.kind {
		case protowire.Fixed32Type:
			values = append(values, float64(math.Float32frombits(uint32(f.u))))
		case protowire.BytesType:
			if len(f.b)%4 != 0 {
				return nil, errors.New("invalid packed float history")
			}
			for i := 0; i < len(f.b); i += 4 {
				values = append(values, float64(math.Float32frombits(binary.LittleEndian.Uint32(f.b[i:]))))
			}
		default:
			return nil, errors.New("unexpected history float type")
		}
	}
	return values, nil
}

var historyKeys = []string{"history_outages", "history_outage_seconds", "history_samples", "last_outage_duration_seconds", "power_watts", "power_mean_watts", "power_min_watts", "power_max_watts", "power_samples"}

// ApplyHistory adds gauges for the returned bounded history, not lifetime totals.
// Raw outage timestamps are retained without assuming an epoch. An invalid
// response leaves the supplied snapshot unchanged.
func ApplyHistory(s *model.Snapshot, data []byte) error {
	if s == nil {
		return errors.New("nil snapshot")
	}
	m, err := response(data, 2006)
	if err != nil {
		return err
	}
	values := make(map[string]float64)
	var outages []model.Outage
	for _, f := range m[1009] {
		if f.kind != protowire.BytesType {
			return errors.New("unexpected outage history type")
		}
		o, err := parse(f.b)
		if err != nil {
			return err
		}
		record, err := decodeOutage(o)
		if err != nil {
			return err
		}
		outages = append(outages, record)
		values["history_outage_seconds"] += record.DurationSeconds
	}
	values["history_outages"] = float64(len(outages))
	if len(outages) == 0 {
		values["history_outage_seconds"] = 0
	}
	sort.SliceStable(outages, func(i, j int) bool { return outages[i].StartNS > outages[j].StartNS })
	if len(outages) > 0 {
		values["last_outage_duration_seconds"] = outages[0].DurationSeconds
	}
	// Keep every record in the size-bounded response so exporters can aggregate
	// the entire returned window by cause. A UI may separately limit its view.
	current, _, err := m.uint(1)
	if err != nil {
		return err
	}
	pings, err := repeatedFloats(m, 1001)
	if err != nil {
		return err
	}
	power, err := repeatedFloats(m, 1010)
	if err != nil {
		return err
	}
	count := uint64(len(pings))
	if current < count {
		count = current
	}
	values["history_samples"] = float64(count)
	if count > 0 && len(power) > 0 {
		var samples []float64
		aligned := true
		for i := uint64(0); i < count; i++ {
			index := (current - count + i) % uint64(len(pings))
			if index >= uint64(len(power)) {
				aligned = false
				break
			}
			samples = append(samples, power[index])
		}
		if aligned {
			var sum, min, max float64
			valid, positive := 0, false
			for _, x := range samples {
				if !finite(x) || x < 0 {
					continue
				}
				if valid == 0 || x < min {
					min = x
				}
				if valid == 0 || x > max {
					max = x
				}
				sum += x
				valid++
				positive = positive || x > 0
			}
			// Unsupported dish hardware can serialize an entire zero-filled array.
			if valid > 0 && positive {
				values["power_mean_watts"] = sum / float64(valid)
				values["power_min_watts"] = min
				values["power_max_watts"] = max
				values["power_samples"] = float64(valid)
				latest := samples[len(samples)-1]
				if finite(latest) && latest >= 0 {
					values["power_watts"] = latest
				}
			}
		}
	}
	if s.Values == nil {
		s.Values = make(map[string]float64)
	}
	for _, key := range historyKeys {
		delete(s.Values, key)
	}
	for key, value := range values {
		s.Values[key] = value
	}
	if s.States == nil {
		s.States = make(map[string]string)
	}
	delete(s.States, "last_outage_cause")
	if len(outages) > 0 {
		s.States["last_outage_cause"] = outages[0].Cause
	}
	s.Outages = outages
	s.HistoryAvailable = true
	s.HistoryError = ""
	return nil
}
