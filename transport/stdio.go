package transport

import (
	"context"
	"os"
)

// StdioTransport implements Transport using stdin/stdout, framed as newline-delimited
// JSON-RPC per the 2026-07-28 stdio binding. It is a thin wrapper around the shared
// streamTransport, which handles concurrent request dispatch (mandatory now that
// subscriptions/listen can hold a request open indefinitely) and notifications/cancelled.
type StdioTransport struct {
	stream *streamTransport
	cancel context.CancelFunc
	done   chan struct{}
}

// NewStdioTransport creates a new stdio transport.
func NewStdioTransport() *StdioTransport {
	return &StdioTransport{stream: newStreamTransport("stdio")}
}

// Start begins reading from stdin and processing messages.
func (t *StdioTransport) Start(handler MessageHandler) error {
	t.stream.handler = handler

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.done = make(chan struct{})

	go func() {
		defer close(t.done)
		t.stream.serve(ctx, os.Stdin, os.Stdout)
	}()

	return nil
}

// Stop signals in-flight requests to cancel and waits for the read loop to exit. Per the
// spec, the primary and only fully portable shutdown signal for a stdio server is its
// stdin being closed by the client; Stop cancels in-flight request contexts but cannot
// interrupt a blocked stdin read itself, so callers that need a hard deadline should race
// this against their own timeout.
func (t *StdioTransport) Stop() error {
	if t.cancel != nil {
		t.cancel()
	}
	<-t.done
	return nil
}
