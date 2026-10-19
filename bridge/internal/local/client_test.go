package local

import (
	"context"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestFetchStatusAndHistory(t *testing.T) {
	var mu sync.Mutex
	var operations []int
	address := serveDish(t, func(ctx context.Context, request []byte) ([]byte, error) {
		op, err := readOperation(request)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		mu.Lock()
		operations = append(operations, op)
		mu.Unlock()
		if op == 1004 {
			return statusResponse(), nil
		}
		return historyResponse(), nil
	})
	client := newTestClient(t, address, time.Second)
	before := time.Now()
	snapshot, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DeviceID != "ut-local-test" || snapshot.HardwareVersion != "rev3_proto2" || snapshot.SoftwareVersion != "test-firmware" {
		t.Fatalf("wrong device info: %+v", snapshot)
	}
	if snapshot.Source != "local" || snapshot.CollectedAt.Before(before) || snapshot.CollectedAt.After(time.Now()) {
		t.Fatalf("wrong collection metadata: source=%q collected_at=%v", snapshot.Source, snapshot.CollectedAt)
	}
	if snapshot.Values["uptime_seconds"] != 7200 || snapshot.Values["downlink_bps"] != 1000000 {
		t.Fatalf("wrong status measurements: %v", snapshot.Values)
	}
	if !snapshot.HistoryAvailable || snapshot.HistoryError != "" || snapshot.Values["power_watts"] != 50 {
		t.Fatalf("history was not applied: %+v", snapshot)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(operations, []int{1004, 1007}) {
		t.Fatalf("expected only status then history, got %v", operations)
	}
}

func TestFetchPreservesStatusWhenHistoryFails(t *testing.T) {
	for _, test := range []struct {
		name    string
		history func(context.Context) ([]byte, error)
	}{
		{"rpc", func(context.Context) ([]byte, error) {
			return nil, status.Error(codes.Unimplemented, "history unavailable")
		}},
		{"protobuf", func(context.Context) ([]byte, error) { return []byte{0xff}, nil }},
		{"timeout", func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := serveDish(t, func(ctx context.Context, request []byte) ([]byte, error) {
				op, err := readOperation(request)
				if err != nil {
					return nil, err
				}
				if op == 1004 {
					return statusResponse(), nil
				}
				return test.history(ctx)
			})
			timeout := time.Second
			if test.name == "timeout" {
				timeout = 500 * time.Millisecond
			}
			client := newTestClient(t, address, timeout)
			snapshot, err := client.Fetch(context.Background())
			if err != nil {
				t.Fatalf("history error discarded status: %v", err)
			}
			if snapshot.DeviceID != "ut-local-test" || snapshot.Values["downlink_bps"] != 1000000 || snapshot.Source != "local" {
				t.Fatalf("status was lost: %+v", snapshot)
			}
			if snapshot.HistoryAvailable || snapshot.HistoryError == "" {
				t.Fatalf("history failure was not recorded: %+v", snapshot)
			}
		})
	}
}

func TestEachRPCHasFullTimeout(t *testing.T) {
	address := serveDish(t, func(ctx context.Context, request []byte) ([]byte, error) {
		op, err := readOperation(request)
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(300 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-timer.C:
		}
		if op == 1004 {
			return statusResponse(), nil
		}
		return historyResponse(), nil
	})
	client := newTestClient(t, address, 500*time.Millisecond)
	snapshot, err := client.Fetch(context.Background())
	if err != nil || !snapshot.HistoryAvailable || snapshot.HistoryError != "" {
		t.Fatalf("both RPCs should have separate timeout budgets: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestStatusTimeoutAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			started := make(chan struct{})
			var calls atomic.Int32
			address := serveDish(t, func(ctx context.Context, request []byte) ([]byte, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-ctx.Done()
				return nil, status.FromContextError(ctx.Err()).Err()
			})
			timeout := 200 * time.Millisecond
			if cancelled {
				timeout = 5 * time.Second
			}
			client := newTestClient(t, address, timeout)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelled {
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			_, err := client.Fetch(ctx)
			want := codes.DeadlineExceeded
			if cancelled {
				want = codes.Canceled
			}
			if status.Code(err) != want {
				t.Fatalf("got error %v, want code %v", err, want)
			}
			if calls.Load() != 1 {
				t.Fatalf("history must not follow failed status; calls=%d", calls.Load())
			}
		})
	}
}

func TestStatusFailureDoesNotFetchHistory(t *testing.T) {
	for _, badProtobuf := range []bool{false, true} {
		t.Run(fmt.Sprintf("bad_protobuf=%v", badProtobuf), func(t *testing.T) {
			var calls atomic.Int32
			address := serveDish(t, func(context.Context, []byte) ([]byte, error) {
				calls.Add(1)
				if badProtobuf {
					return []byte{0xff}, nil
				}
				return nil, status.Error(codes.Unavailable, "dish offline")
			})
			client := newTestClient(t, address, time.Second)
			if _, err := client.Fetch(context.Background()); err == nil {
				t.Fatal("expected status failure")
			}
			if calls.Load() != 1 {
				t.Fatalf("history must not follow failed status; calls=%d", calls.Load())
			}
		})
	}
}

func TestClose(t *testing.T) {
	address := serveDish(t, func(context.Context, []byte) ([]byte, error) { return statusResponse(), nil })
	client := newTestClient(t, address, time.Second)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded after Close")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	for _, test := range []struct {
		address string
		timeout time.Duration
	}{{"", time.Second}, {"  ", time.Second}, {"localhost:9200", 0}, {"localhost:9200", -time.Second}} {
		if client, err := New(test.address, test.timeout); err == nil {
			client.Close()
			t.Errorf("New(%q, %v) succeeded", test.address, test.timeout)
		}
	}
}

func newTestClient(t *testing.T, address string, timeout time.Duration) *Client {
	t.Helper()
	client, err := New(address, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// Establish the connection outside the RPC's timeout budget. These tests
	// measure handler deadlines, not resolver/dial scheduling during compilation.
	client.conn.Connect()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for state := client.conn.GetState(); state != connectivity.Ready; state = client.conn.GetState() {
		if !client.conn.WaitForStateChange(ctx, state) {
			t.Fatalf("mock gRPC connection did not become ready: %v", ctx.Err())
		}
	}
	return client
}

type deviceServer interface {
	Handle(context.Context, []byte) ([]byte, error)
}

type testDish struct {
	handle func(context.Context, []byte) ([]byte, error)
}

func (d *testDish) Handle(ctx context.Context, request []byte) ([]byte, error) {
	return d.handle(ctx, request)
}

func serveDish(t *testing.T, handle func(context.Context, []byte) ([]byte, error)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}))
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "SpaceX.API.Device.Device",
		HandlerType: (*deviceServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Handle",
			Handler: func(srv any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				var request []byte
				if err := decode(&request); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(deviceServer).Handle(ctx, request)
				}
				return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: srv, FullMethod: handleMethod}, func(ctx context.Context, req any) (any, error) {
					return srv.(deviceServer).Handle(ctx, req.([]byte))
				})
			},
		}},
	}, &testDish{handle: handle})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

// Validate the wire independently of the request builders: only an empty local
// target and one empty, read-only status/history request may be sent.
func readOperation(request []byte) (int, error) {
	op := 0
	for len(request) > 0 {
		field, wire, n := protowire.ConsumeTag(request)
		if n < 0 || wire != protowire.BytesType {
			return 0, fmt.Errorf("unexpected request field or wire type")
		}
		request = request[n:]
		value, n := protowire.ConsumeBytes(request)
		if n < 0 {
			return 0, fmt.Errorf("malformed request")
		}
		request = request[n:]
		if field == 13 && len(value) == 0 {
			continue
		}
		if (field != 1004 && field != 1007) || len(value) != 0 || op != 0 {
			return 0, fmt.Errorf("unexpected request field %d", field)
		}
		op = int(field)
	}
	if op == 0 {
		return 0, fmt.Errorf("missing operation")
	}
	return op, nil
}

func bytesField(dst []byte, field protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(dst, field, protowire.BytesType), value)
}

func statusResponse() []byte {
	var info []byte
	info = bytesField(info, 1, []byte("ut-local-test"))
	info = bytesField(info, 2, []byte("rev3_proto2"))
	info = bytesField(info, 3, []byte("test-firmware"))
	state := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 7200)
	var dish []byte
	dish = bytesField(dish, 1, info)
	dish = bytesField(dish, 2, state)
	for _, pair := range []struct {
		field protowire.Number
		value float32
	}{{1003, .125}, {1007, 1000000}, {1008, 100000}, {1009, 25}} {
		dish = protowire.AppendFixed32(protowire.AppendTag(dish, pair.field, protowire.Fixed32Type), math.Float32bits(pair.value))
	}
	return bytesField(nil, 2004, dish)
}

func historyResponse() []byte {
	dish := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 3)
	var powers, drops []byte
	for _, power := range []float32{30, 40, 50} {
		powers = protowire.AppendFixed32(powers, math.Float32bits(power))
		drops = protowire.AppendFixed32(drops, 0)
	}
	dish = bytesField(dish, 1001, drops)
	dish = bytesField(dish, 1010, powers)
	return bytesField(nil, 2006, dish)
}

func TestRawCodecRejectsWrongTypes(t *testing.T) {
	codec := rawCodec{}
	if _, err := codec.Marshal("not bytes"); err == nil {
		t.Fatal("accepted a non-byte message")
	}
	var nilPointer *[]byte
	if err := codec.Unmarshal(nil, nilPointer); err == nil || !strings.Contains(err.Error(), "non-nil") {
		t.Fatalf("accepted nil destination: %v", err)
	}
}
