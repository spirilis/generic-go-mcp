package transport

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/spirilis/generic-go-mcp/logging"
)

// UnixTransportConfig holds configuration for UNIX socket transport
type UnixTransportConfig struct {
	SocketPath string
	FileMode   os.FileMode
}

// UnixTransport implements Transport using UNIX domain sockets. Per the 2026-07-28 spec,
// custom transports over a reliable bidirectional byte stream SHOULD reuse the stdio
// newline-delimited JSON-RPC framing rather than defining a new one; this transport does,
// via the shared streamTransport binding, so it inherits concurrent dispatch and
// notifications/cancelled handling for free.
type UnixTransport struct {
	config   UnixTransportConfig
	listener net.Listener
	handler  MessageHandler
	stopCh   chan struct{}
	wg       sync.WaitGroup

	connMu     sync.Mutex
	conn       net.Conn
	cancelConn context.CancelFunc
}

// NewUnixTransport creates a new UNIX socket transport
func NewUnixTransport(config UnixTransportConfig) *UnixTransport {
	return &UnixTransport{
		config: config,
		stopCh: make(chan struct{}),
	}
}

// Start begins listening on the UNIX socket
func (t *UnixTransport) Start(handler MessageHandler) error {
	t.handler = handler

	// Remove existing socket file if it exists
	if err := os.Remove(t.config.SocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create UNIX listener
	listener, err := net.Listen("unix", t.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket: %w", err)
	}
	t.listener = listener

	// Set socket file permissions
	if err := os.Chmod(t.config.SocketPath, t.config.FileMode); err != nil {
		t.listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	logging.Info("UNIX socket listening", "path", t.config.SocketPath, "mode", fmt.Sprintf("%04o", t.config.FileMode))

	// Start accept loop
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.acceptLoop()
	}()

	return nil
}

// Stop gracefully stops the transport and cleans up the socket
func (t *UnixTransport) Stop() error {
	close(t.stopCh)

	// Close the listener to stop accepting new connections
	if t.listener != nil {
		t.listener.Close()
	}

	// Close any active connection and cancel its in-flight requests
	t.connMu.Lock()
	if t.conn != nil {
		t.conn.Close()
	}
	if t.cancelConn != nil {
		t.cancelConn()
	}
	t.connMu.Unlock()

	// Wait for goroutines to finish
	t.wg.Wait()

	// Remove socket file
	if err := os.Remove(t.config.SocketPath); err != nil && !os.IsNotExist(err) {
		logging.Warn("Failed to remove socket file", "path", t.config.SocketPath, "error", err)
	}

	return nil
}

// acceptLoop accepts connections from the socket. Only one connection is served at a
// time: a new connection closes and replaces whatever the previous one was doing, same
// as before this migration.
func (t *UnixTransport) acceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.stopCh:
				return
			default:
				logging.Error("Error accepting connection", "error", err)
				continue
			}
		}

		ctx, cancel := context.WithCancel(context.Background())

		t.connMu.Lock()
		if t.conn != nil {
			t.conn.Close()
		}
		if t.cancelConn != nil {
			t.cancelConn()
		}
		t.conn = conn
		t.cancelConn = cancel
		t.connMu.Unlock()

		logging.Debug("Client connected to UNIX socket")

		t.wg.Add(1)
		go func(c net.Conn, ctx context.Context, cancel context.CancelFunc) {
			defer t.wg.Done()
			defer c.Close()
			defer cancel()

			stream := newStreamTransport("unix")
			stream.handler = t.handler
			stream.serve(ctx, c, c)

			logging.Debug("Client disconnected from UNIX socket")
		}(conn, ctx, cancel)
	}
}
