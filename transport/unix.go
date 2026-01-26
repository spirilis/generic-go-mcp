package transport

import (
	"bufio"
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

// UnixTransport implements Transport using UNIX domain sockets
type UnixTransport struct {
	config   UnixTransportConfig
	listener net.Listener
	handler  MessageHandler
	stopCh   chan struct{}
	wg       sync.WaitGroup
	connMu   sync.Mutex
	conn     net.Conn
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

	// Close any active connection
	t.connMu.Lock()
	if t.conn != nil {
		t.conn.Close()
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

// acceptLoop accepts connections from the socket
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

		// Handle one connection at a time
		t.connMu.Lock()
		if t.conn != nil {
			// Close previous connection if one exists
			t.conn.Close()
		}
		t.conn = conn
		t.connMu.Unlock()

		logging.Debug("Client connected to UNIX socket")

		// Handle the connection in a goroutine
		t.wg.Add(1)
		go func(c net.Conn) {
			defer t.wg.Done()
			defer c.Close()
			t.handleConnection(c)
		}(conn)
	}
}

// handleConnection processes messages from a single connection
func (t *UnixTransport) handleConnection(conn net.Conn) {
	scanner := bufio.NewScanner(conn)

	for {
		select {
		case <-t.stopCh:
			return
		default:
			if !scanner.Scan() {
				// Check for error or EOF
				if err := scanner.Err(); err != nil {
					logging.Error("Scanner error", "error", err)
				} else {
					logging.Debug("Client disconnected from UNIX socket")
				}
				return
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			// Handle the message
			response := t.handler.HandleMessage(line)

			// Write response back to the client
			if response != nil {
				if _, err := conn.Write(append(response, '\n')); err != nil {
					logging.Error("Error writing response", "error", err)
					return
				}
			}
		}
	}
}
