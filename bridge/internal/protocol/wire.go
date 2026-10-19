// Package protocol implements the two read-only Starlink protobuf operations.
// Field definitions are pinned to Eitol/starlink-client commit
// a9164c8cf24774c1137d9c28680d8247bf6849eb, proto/spacex/api/device.
package protocol

import (
	"errors"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

const maxMessageSize = 4 << 20

type wireField struct {
	kind protowire.Type
	u    uint64
	b    []byte
}
type message map[protowire.Number][]wireField

func parse(data []byte) (message, error) {
	if len(data) > maxMessageSize {
		return nil, errors.New("protobuf response exceeds size limit")
	}
	m := make(message)
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 || !num.IsValid() {
			return nil, errors.New("invalid protobuf field tag")
		}
		data = data[n:]
		f := wireField{kind: typ}
		switch typ {
		case protowire.VarintType:
			f.u, n = protowire.ConsumeVarint(data)
		case protowire.Fixed32Type:
			var u uint32
			u, n = protowire.ConsumeFixed32(data)
			f.u = uint64(u)
		case protowire.Fixed64Type:
			f.u, n = protowire.ConsumeFixed64(data)
		case protowire.BytesType:
			f.b, n = protowire.ConsumeBytes(data)
		default:
			return nil, errors.New("unsupported protobuf wire type")
		}
		if n < 0 {
			return nil, errors.New("truncated or invalid protobuf field")
		}
		m[num] = append(m[num], f)
		data = data[n:]
	}
	return m, nil
}

func (m message) last(num protowire.Number, kind protowire.Type) (wireField, bool, error) {
	fs := m[num]
	if len(fs) == 0 {
		return wireField{}, false, nil
	}
	for _, f := range fs {
		if f.kind != kind {
			return wireField{}, false, errors.New("unexpected protobuf field type")
		}
	}
	return fs[len(fs)-1], true, nil
}

// Singular embedded messages merge across repeated occurrences in protobuf.
func (m message) nested(num protowire.Number) (message, bool, error) {
	fs := m[num]
	if len(fs) == 0 {
		return nil, false, nil
	}
	var data []byte
	for _, f := range fs {
		if f.kind != protowire.BytesType {
			return nil, false, errors.New("unexpected protobuf message type")
		}
		data = append(data, f.b...)
	}
	x, err := parse(data)
	return x, true, err
}

func (m message) uint(num protowire.Number) (uint64, bool, error) {
	f, ok, err := m.last(num, protowire.VarintType)
	return f.u, ok, err
}

func (m message) text(num protowire.Number) (string, error) {
	f, _, err := m.last(num, protowire.BytesType)
	return string(f.b), err
}

func (m message) float(num protowire.Number) (float64, bool, error) {
	f, ok, err := m.last(num, protowire.Fixed32Type)
	x := float64(math.Float32frombits(uint32(f.u)))
	return x, ok && finite(x), err
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func appendMessage(dst []byte, num protowire.Number, data []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(dst, num, protowire.BytesType), data)
}

func readRequest(target string, num protowire.Number) []byte {
	var data []byte
	if target != "" {
		data = appendMessage(data, 13, []byte(target))
	}
	return appendMessage(data, num, nil)
}

// StatusRequest returns raw protobuf containing only an optional target and get_status.
func StatusRequest(target string) []byte { return readRequest(target, 1004) }

// HistoryRequest returns raw protobuf containing only an optional target and get_history.
func HistoryRequest(target string) []byte { return readRequest(target, 1007) }

func response(data []byte, num protowire.Number) (message, error) {
	m, err := parse(data)
	if err != nil {
		return nil, err
	}
	status, _, err := m.nested(2)
	if err != nil {
		return nil, err
	}
	code, _, err := status.uint(1)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New("dish rejected the read request")
	}
	dish, ok, err := m.nested(num)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("response does not contain the requested dish data")
	}
	return dish, nil
}
