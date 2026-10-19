package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// RPCError carries the standard transport status without server-provided text.
// Callers may distinguish authentication expiry from a device/transport failure.
type RPCError struct{ Code int }

func (e *RPCError) Error() string {
	return fmt.Sprintf("cloud gRPC request failed (status %d)", e.Code)
}

func completionCode(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("invalid gRPC completion status")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid gRPC completion status")
		}
	}
	code, err := strconv.Atoi(value)
	if err != nil || code > 16 {
		return 0, errors.New("invalid gRPC completion status")
	}
	return code, nil
}

// EncodeWebFrame wraps raw protobuf in one uncompressed binary gRPC-Web frame.
func EncodeWebFrame(payload []byte) []byte {
	data := make([]byte, 5, len(payload)+5)
	binary.BigEndian.PutUint32(data[1:], uint32(len(payload)))
	return append(data, payload...)
}

// DecodeWebResponse validates the unary gRPC-Web completion status as well as
// frame boundaries. Server error text is deliberately excluded from errors.
func DecodeWebResponse(data []byte, headers http.Header) ([]byte, error) {
	if len(data) > maxMessageSize {
		return nil, errors.New("gRPC-Web response exceeds size limit")
	}
	var payload []byte
	dataSeen, trailerSeen := false, false
	var trailerStatus string
	statusSeen := false
	for len(data) > 0 {
		if trailerSeen {
			return nil, errors.New("frame after gRPC-Web trailers")
		}
		if len(data) < 5 {
			return nil, errors.New("truncated gRPC-Web frame header")
		}
		flag, size := data[0], uint64(binary.BigEndian.Uint32(data[1:5]))
		data = data[5:]
		if size > uint64(len(data)) {
			return nil, errors.New("truncated gRPC-Web frame")
		}
		body := data[:int(size)]
		data = data[int(size):]
		switch flag {
		case 0:
			if dataSeen {
				return nil, errors.New("multiple messages in unary gRPC-Web response")
			}
			dataSeen = true
			payload = body
		case 128:
			trailerSeen = true
			for _, c := range body {
				if c > 127 {
					return nil, errors.New("invalid gRPC-Web trailers")
				}
			}
			for _, line := range strings.Split(string(body), "\r\n") {
				if line == "" {
					continue
				}
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					return nil, errors.New("invalid gRPC-Web trailer line")
				}
				if strings.EqualFold(strings.TrimSpace(key), "grpc-status") {
					if statusSeen {
						return nil, errors.New("duplicate gRPC completion status")
					}
					statusSeen = true
					trailerStatus = strings.TrimSpace(value)
				}
			}
		default:
			return nil, errors.New("compressed or invalid gRPC-Web frame")
		}
	}
	var headerStatuses []string
	for key, values := range headers {
		if strings.EqualFold(key, "grpc-status") {
			headerStatuses = append(headerStatuses, values...)
		}
	}
	if len(headerStatuses) > 1 {
		return nil, errors.New("duplicate gRPC completion status")
	}
	if len(headerStatuses) == 0 && !statusSeen {
		return nil, errors.New("missing gRPC completion status")
	}
	code := -1
	if len(headerStatuses) == 1 {
		var err error
		code, err = completionCode(headerStatuses[0])
		if err != nil {
			return nil, err
		}
	}
	if statusSeen {
		trailerCode, err := completionCode(trailerStatus)
		if err != nil {
			return nil, err
		}
		if code >= 0 && code != trailerCode {
			return nil, errors.New("conflicting gRPC completion status")
		}
		code = trailerCode
	}
	if code != 0 {
		return nil, &RPCError{Code: code}
	}
	if !dataSeen {
		return nil, errors.New("missing gRPC-Web response message")
	}
	return payload, nil
}
