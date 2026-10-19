package protocol

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func uintField(n protowire.Number, x uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), x)
}
func floatField(n protowire.Number, x float32) []byte {
	return protowire.AppendFixed32(protowire.AppendTag(nil, n, protowire.Fixed32Type), math.Float32bits(x))
}
func msg(n protowire.Number, parts ...[]byte) []byte {
	return appendMessage(nil, n, bytes.Join(parts, nil))
}
func packed(n protowire.Number, xs ...float32) []byte {
	var b []byte
	for _, x := range xs {
		b = protowire.AppendFixed32(b, math.Float32bits(x))
	}
	return msg(n, b)
}

func TestReadRequestsGolden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		got    []byte
		golden string
	}{
		{"status targeted", StatusRequest("utabc"), "6a057574616263e23e00"},
		{"history local", HistoryRequest(""), "fa3e00"},
		{"status local", StatusRequest(""), "e23e00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, hexBytes(t, tc.golden)) {
				t.Fatalf("got %x", tc.got)
			}
		})
	}
	got := EncodeWebFrame(StatusRequest("utabc"))
	if !bytes.Equal(got, hexBytes(t, "000000000a6a057574616263e23e00")) {
		t.Fatalf("frame %x", got)
	}
}

func trailer(s string) []byte { b := EncodeWebFrame([]byte(s)); b[0] = 128; return b }

func TestWebResponseValidation(t *testing.T) {
	good := append(EncodeWebFrame([]byte{1, 2}), trailer("grpc-status: 0\r\n")...)
	got, err := DecodeWebResponse(good, nil)
	if err != nil || !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("valid frame %x %v", got, err)
	}
	if _, err = DecodeWebResponse(EncodeWebFrame([]byte{1}), http.Header{"Grpc-Status": {"0"}}); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated header":         {0, 0},
		"truncated body":           {0, 0, 0, 0, 2, 1},
		"missing completion":       EncodeWebFrame([]byte{1}),
		"compressed":               {1, 0, 0, 0, 0},
		"error trailer":            append(EncodeWebFrame([]byte{1}), trailer("grpc-status: 7\r\ngrpc-message: secret\r\n")...),
		"duplicate status":         append(EncodeWebFrame([]byte{1}), trailer("grpc-status: 7\r\ngrpc-status: 0\r\n")...),
		"data after empty trailer": append(trailer(""), good...),
		"second trailer":           append(good, trailer("grpc-status: 0\r\n")...),
		"multiple messages":        append(EncodeWebFrame(nil), good...),
		"trailers only":            trailer("grpc-status: 0\r\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeWebResponse(data, nil)
			if err == nil {
				t.Fatal("accepted malformed frame")
			}
			if bytes.Contains([]byte(err.Error()), []byte("secret")) {
				t.Fatal("server error text leaked")
			}
		})
	}
	if _, err := DecodeWebResponse(good, http.Header{"Grpc-Status": {"13"}}); err == nil {
		t.Fatal("accepted conflicting HTTP status")
	}
}

func TestWebAuthenticationStatus(t *testing.T) {
	for _, code := range []string{"7", "16"} {
		_, err := DecodeWebResponse(trailer("grpc-status: "+code+"\r\ngrpc-message: private-session-details\r\n"), nil)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || fmt.Sprint(rpcErr.Code) != code {
			t.Fatalf("status %s: %v", code, err)
		}
		if strings.Contains(err.Error(), "private") {
			t.Fatal("server-provided text leaked")
		}
	}
	for _, code := range []string{"-1", "17", "+0", "not-a-code", ""} {
		_, err := DecodeWebResponse(trailer("grpc-status: "+code+"\r\n"), nil)
		var rpcErr *RPCError
		if err == nil || errors.As(err, &rpcErr) {
			t.Fatalf("invalid status %q became valid RPC error", code)
		}
	}
	if _, err := DecodeWebResponse(trailer("grpc-status: 16\r\n"), http.Header{"Grpc-Status": {"7"}}); err == nil {
		t.Fatal("inconsistent status accepted")
	}
}

func statusFixture(parts ...[]byte) []byte {
	return msg(2004, append([][]byte{msg(1, msg(1, []byte("ut-test")), msg(2, []byte("rev3_proto2")), msg(3, []byte("firmware")))}, parts...)...)
}

func TestStatusUnitsDefaultsAndClearedAlerts(t *testing.T) {
	b := statusFixture(msg(2, uintField(1, 123)), floatField(1007, 2000000), floatField(1008, 400000), floatField(1009, 12.5), floatField(1003, .25), msg(1004, floatField(1, .125)), msg(1005, uintField(9, 1)), floatField(1011, 170), floatField(1012, 60), msg(1027, floatField(3, 4)), msg(1026, uintField(1, 6), uintField(3, 1)))
	s, err := DecodeStatus(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.DeviceID != "ut-test" || s.HardwareVersion != "rev3_proto2" {
		t.Fatalf("identity %+v", s)
	}
	for key, want := range map[string]float64{"downlink_bps": 2000000, "uplink_bps": 400000, "latency_seconds": .0125, "ping_drop_ratio": .25, "obstruction_ratio": .125, "uptime_seconds": 123, "heating": 1, "thermal_shutdown": 0, "currently_obstructed": 0, "current_outage": 0, "azimuth_degrees": 170, "elevation_degrees": 60, "tilt_degrees": 4, "update_reboot_required": 1} {
		if s.Values[key] != want {
			t.Errorf("%s got %v want %v", key, s.Values[key], want)
		}
	}
	if s.States["software_update_state"] != "REBOOT_REQUIRED" {
		t.Fatal(s.States)
	}
	cleared, err := DecodeStatus(statusFixture(msg(1005)))
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := cleared.Values["heating"]; !ok || x != 0 {
		t.Fatal("empty alerts failed to clear heating")
	}
	if _, ok := cleared.Values["power_watts"]; ok {
		t.Fatal("invented power")
	}
	if _, ok := cleared.Values["signal_above_noise_floor"]; ok {
		t.Fatal("invented signal support")
	}
	if _, ok := cleared.Values["obstruction_ratio"]; ok {
		t.Fatal("invented absent parent")
	}
	if cleared.States["current_outage_cause"] != "NONE" || cleared.Values["current_outage_duration_seconds"] != 0 {
		t.Fatal("no-current-outage state failed to clear")
	}
	for _, key := range []string{"downlink_bps", "uplink_bps", "latency_seconds", "ping_drop_ratio"} {
		if x, ok := cleared.Values[key]; !ok || x != 0 {
			t.Errorf("proto3 default %s", key)
		}
	}
}

func TestStatusOutageAndMalformedFields(t *testing.T) {
	s, err := DecodeStatus(statusFixture(msg(1014, uintField(1, 6), uintField(2, math.MaxUint64), uintField(3, 2500000000))))
	if err != nil {
		t.Fatal(err)
	}
	if s.Values["current_outage"] != 1 || s.Values["current_outage_duration_seconds"] != 2.5 || s.States["current_outage_cause"] != "OBSTRUCTED" {
		t.Fatalf("bad outage %+v", s)
	}
	cases := [][]byte{{0xff}, msg(2, uintField(1, 7)), msg(2006), msg(2004), statusFixture(uintField(1009, 10)), statusFixture(msg(1005, floatField(9, 1)))}
	for _, b := range cases {
		if _, err := DecodeStatus(b); err == nil {
			t.Fatalf("accepted malformed status %x", b)
		}
	}
	s, err = DecodeStatus(statusFixture(floatField(1009, float32(math.NaN()))))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Values["latency_seconds"]; ok {
		t.Fatal("serialized NaN became a measurement")
	}
}

func historyFixture(current uint64, pings, power []float32, extras ...[]byte) []byte {
	parts := [][]byte{uintField(1, current), packed(1001, pings...), packed(1010, power...)}
	return msg(2006, append(parts, extras...)...)
}

func TestHistoryRingPowerAndOutages(t *testing.T) {
	outage := func(start uint64) []byte {
		return msg(1009, uintField(1, 6), uintField(2, start), uintField(3, 1500000000))
	}
	s := model.Snapshot{}
	if err := ApplyHistory(&s, historyFixture(7, []float32{0, 0, 0, 0}, []float32{80, 90, 60, 70}, outage(10), outage(30), outage(20))); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"history_samples": 4, "history_outages": 3, "history_outage_seconds": 4.5, "power_watts": 60, "power_mean_watts": 75, "power_min_watts": 60, "power_max_watts": 90, "power_samples": 4} {
		if s.Values[key] != want {
			t.Errorf("%s got %v want %v", key, s.Values[key], want)
		}
	}
	if !s.HistoryAvailable || len(s.Outages) != 3 || s.Outages[0].StartNS != 30 || s.Outages[2].StartNS != 10 {
		t.Fatalf("history %+v", s)
	}
	if s.States["last_outage_cause"] != "OBSTRUCTED" || s.Values["last_outage_duration_seconds"] != 1.5 {
		t.Fatal("missing newest outage summary")
	}
	if err := ApplyHistory(&s, historyFixture(2, []float32{0, 0, 0, 0}, []float32{20, 30, 999, 999})); err != nil {
		t.Fatal(err)
	}
	if s.Values["power_watts"] != 30 || s.Values["power_mean_watts"] != 25 || len(s.Outages) != 0 {
		t.Fatal(s.Values)
	}
	if _, ok := s.States["last_outage_cause"]; ok {
		t.Fatal("old outage cause survived an empty history")
	}
	if _, ok := s.Values["last_outage_duration_seconds"]; ok {
		t.Fatal("old outage duration survived an empty history")
	}
	if err := ApplyHistory(&s, historyFixture(7, []float32{0, 0, 0, 0}, []float32{0, 0, 0, 0})); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"power_watts", "power_mean_watts", "power_samples"} {
		if _, ok := s.Values[key]; ok {
			t.Fatalf("unsupported power left %s", key)
		}
	}
}

func TestHistoryRepeatedChunksAndInvalidIsAtomic(t *testing.T) {
	s := model.Snapshot{Values: map[string]float64{"heating": 1}}
	b := msg(2006, uintField(1, 3), packed(1001, 0), packed(1001, 0, 0), packed(1010, 10), floatField(1010, 20), packed(1010, 30))
	if err := ApplyHistory(&s, b); err != nil {
		t.Fatal(err)
	}
	if s.Values["power_watts"] != 30 || s.Values["power_mean_watts"] != 20 {
		t.Fatal(s.Values)
	}
	before := make(map[string]float64)
	for k, v := range s.Values {
		before[k] = v
	}
	if err := ApplyHistory(&s, msg(2006, msg(1010, []byte{1, 2, 3}))); err == nil {
		t.Fatal("accepted misaligned packed floats")
	}
	if !reflect.DeepEqual(before, s.Values) {
		t.Fatal("failed history mutated snapshot")
	}
	if err := ApplyHistory(&s, historyFixture(10, []float32{0, 0, 0}, []float32{12})); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Values["power_watts"]; ok {
		t.Fatal("short power array treated as independent ring")
	}
	if err := ApplyHistory(nil, b); err == nil {
		t.Fatal("nil snapshot accepted")
	}
}

func TestNewestOutageSummaryUsesRawTimestampOrder(t *testing.T) {
	s := model.Snapshot{}
	b := msg(2006,
		msg(1009, uintField(1, 6), uintField(2, 90), uintField(3, 2000000000)),
		msg(1009, uintField(1, 3), uintField(2, 20), uintField(3, 5000000000)),
	)
	if err := ApplyHistory(&s, b); err != nil {
		t.Fatal(err)
	}
	if s.States["last_outage_cause"] != "OBSTRUCTED" || s.Values["last_outage_duration_seconds"] != 2 {
		t.Fatalf("wrong newest outage: %+v", s)
	}
}

func TestHistoryPreservesFullWindowAndSignedTimestamp(t *testing.T) {
	var rows [][]byte
	for i := uint64(0); i < 25; i++ {
		rows = append(rows, msg(1009, uintField(1, 1), uintField(2, i), uintField(3, 1000000000)))
	}
	s := model.Snapshot{}
	if err := ApplyHistory(&s, msg(2006, rows...)); err != nil {
		t.Fatal(err)
	}
	if len(s.Outages) != 25 || s.Values["history_outages"] != 25 || s.Values["history_outage_seconds"] != 25 {
		t.Fatalf("full window records %+v", s)
	}
	var causeCount, causeDuration float64
	for _, row := range s.Outages {
		if row.Cause == "BOOTING" {
			causeCount++
			causeDuration += row.DurationSeconds
		}
	}
	if causeCount != s.Values["history_outages"] || causeDuration != s.Values["history_outage_seconds"] {
		t.Fatal("per-cause aggregation lost records")
	}
	if err := ApplyHistory(&s, msg(2006, msg(1009, uintField(2, math.MaxUint64)))); err != nil {
		t.Fatal(err)
	}
	if s.Outages[0].StartNS != -1 {
		t.Fatal("signed timestamp lost")
	}
}
