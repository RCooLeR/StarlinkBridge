// Package local reads a Starlink dish through its local gRPC endpoint.
package local

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
	"github.com/RCooLeR/StarlinkBridge/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const handleMethod = "/SpaceX.API.Device.Device/Handle"

// Client owns a reusable connection to the dish. Each RPC has its own timeout.
type Client struct {
	conn      *grpc.ClientConn
	timeout   time.Duration
	closeOnce sync.Once
	closeErr  error
}

// New configures a local connection. grpc.NewClient connects lazily on Fetch.
func New(address string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("local gRPC address is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("local gRPC timeout must be positive")
	}
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("configure local gRPC connection: %w", err)
	}
	return &Client{conn: conn, timeout: timeout}, nil
}

// Fetch reads status and then history. A history failure does not discard status.
func (c *Client) Fetch(ctx context.Context) (model.Snapshot, error) {
	status, err := c.invoke(ctx, protocol.StatusRequest(""))
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("local status RPC: %w", err)
	}
	snapshot, err := protocol.DecodeStatus(status)
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("decode local status: %w", err)
	}
	snapshot.Source = "local"
	snapshot.CollectedAt = time.Now().UTC()

	history, err := c.invoke(ctx, protocol.HistoryRequest(""))
	if err != nil {
		snapshot.HistoryError = fmt.Sprintf("local history RPC: %v", err)
		return snapshot, nil
	}
	if err := protocol.ApplyHistory(&snapshot, history); err != nil {
		snapshot.HistoryAvailable = false
		snapshot.HistoryError = fmt.Sprintf("decode local history: %v", err)
	}
	return snapshot, nil
}

func (c *Client) invoke(ctx context.Context, request []byte) ([]byte, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var response []byte
	if err := c.conn.Invoke(rpcCtx, handleMethod, request, &response); err != nil {
		return nil, err
	}
	return response, nil
}

// Close releases the connection. Repeated calls return the first close result.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.conn.Close() })
	return c.closeErr
}

// rawCodec leaves protobuf encoding to protocol without registering a global
// codec or requiring generated Starlink message types.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(value any) ([]byte, error) {
	data, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("raw protobuf codec requires []byte, got %T", value)
	}
	return data, nil
}

func (rawCodec) Unmarshal(data []byte, value any) error {
	dst, ok := value.(*[]byte)
	if !ok || dst == nil {
		return fmt.Errorf("raw protobuf codec requires a non-nil *[]byte, got %T", value)
	}
	*dst = append((*dst)[:0], data...)
	return nil
}
