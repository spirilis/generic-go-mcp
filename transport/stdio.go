package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/spirilis/generic-go-mcp/logging"
)

// StdioTransport implements Transport using a pair of byte streams — by default the
// process's own stdin/stdout — framed as newline-delimited JSON-RPC per the 2026-07-28
// stdio binding. It is a thin wrapper around the shared streamTransport, which handles
// concurrent request dispatch (mandatory now that subscriptions/listen can hold a request
// open indefinitely) and notifications/cancelled.
//
// Unlike the other transports, a stdio server's run has a natural end: its input reaching
// EOF is the portable signal that the client is done with it. Done reports that, and Err
// says whether it was a clean EOF or a read failure.
type StdioTransport struct {
	stream *streamTransport
	in     io.Reader
	out    io.Writer

	// ctx/cancel are the transport's own lifetime, not any request's, which is why they
	// live on the struct rather than being passed in. Creating them in the constructor
	// rather than in Start means Stop never races Start over the cancel func, and needs no
	// nil check.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed once the read loop has exited; err is written before that close, so a
	// receive from done happens-after the write and Err needs no lock of its own.
	done chan struct{}
	err  error

	startOnce sync.Once
}

// StdioTransport is currently the only Transport whose run ends on its own.
var _ DoneNotifier = (*StdioTransport)(nil)

// NewStdioTransport creates a stdio transport over the process's own stdin/stdout. This is
// what a desktop MCP client launching this server as a subprocess expects.
func NewStdioTransport() *StdioTransport {
	return NewStdioTransportWithStreams(os.Stdin, os.Stdout)
}

// NewStdioTransportWithStreams creates a stdio transport over arbitrary streams rather than
// the process's stdin/stdout — for tests (a pair of io.Pipe), or for a host that has already
// wrapped or redirected the streams it wants to serve on. A nil in or out means the process
// default, matching logging.Initialize's treatment of a nil io.Writer.
//
// The framing, dispatch and lifecycle are identical either way; only where the bytes come
// from and go to differs.
func NewStdioTransportWithStreams(in io.Reader, out io.Writer) *StdioTransport {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StdioTransport{
		stream: newStreamTransport("stdio"),
		in:     in,
		out:    out,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// Start begins reading from the input stream and processing messages. It launches the read
// loop in its own goroutine and returns immediately.
//
// A transport is single-use: the shared stream binding is safe to serve once, so a second
// Start returns an error rather than silently leaking a second read loop. Calling Stop first
// and Start afterwards yields an already-cancelled transport, since Stop cancels the
// transport's whole lifetime — construct a fresh one instead.
func (t *StdioTransport) Start(handler MessageHandler) error {
	started := false

	t.startOnce.Do(func() {
		started = true
		t.stream.handler = handler

		go func() {
			err := t.stream.serve(t.ctx, t.in, t.out)
			// Write err before closing done: Err's contract is that it is valid once done
			// is closed, and the close is what publishes the write.
			t.err = err
			if err != nil {
				logging.Debug("stdio read loop failed", "transport", "stdio", "error", err)
			} else {
				logging.Debug("stdio input closed", "transport", "stdio")
			}
			close(t.done)
		}()
	})

	if !started {
		return errors.New("transport: StdioTransport already started")
	}
	return nil
}

// Stop cancels every in-flight request and returns. It deliberately does not wait for the
// read loop: that loop is parked in a blocking read on the input stream, which no portable
// mechanism can interrupt, so waiting on it would mean waiting for the client to close its
// end — which is exactly what Done is for.
//
// Callers wanting the old blocking behaviour write:
//
//	trans.Stop()
//	<-trans.Done()
//
// and callers wanting a hard deadline select Done against their own timer:
//
//	trans.Stop()
//	select {
//	case <-trans.Done():
//	case <-time.After(2 * time.Second):
//	}
//
// Safe to call more than once, and safe to call without a preceding Start.
func (t *StdioTransport) Stop() error {
	t.cancel()
	return nil
}

// Done returns a channel closed once the read loop has exited — because the input reached
// EOF, because reading it failed, or because Stop was called and the loop unwound. For a
// stdio server, the client closing the input is the portable shutdown signal, so this is
// the channel to select on alongside SIGINT/SIGTERM to exit cleanly.
//
// The shared stream binding waits for every in-flight handler to return before it exits, so
// a closed Done also means the server has finished draining. Safe to call at any time,
// including before Start: the channel is allocated by the constructor and a receive simply
// blocks until the transport actually runs and finishes.
//
// Use Err to find out whether the run ended cleanly.
func (t *StdioTransport) Done() <-chan struct{} {
	return t.done
}

// Err reports why the read loop ended: nil for a clean EOF on the input or a deliberate
// Stop, non-nil if reading the input itself failed. A consumer choosing a process exit code
// wants this — a clean EOF is exit 0, a read failure is not.
//
// Only valid once Done is closed; before that it returns nil, which is indistinguishable
// from a clean exit.
func (t *StdioTransport) Err() error {
	return t.err
}
