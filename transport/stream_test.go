package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// streamTestHandler simulates a stateless MCP server for exercising the shared
// stream binding: subscriptions/listen acknowledges immediately and then blocks until
// its context is cancelled (by notifications/cancelled or the connection closing);
// every other method responds immediately, echoing its own method name.
type streamTestHandler struct{}

func (streamTestHandler) HandleMessage(ctx context.Context, data []byte, w ResponseWriter) {
	var req JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}
	switch req.Method {
	case "subscriptions/listen":
		w.WriteNotification("notifications/subscriptions/acknowledged", map[string]interface{}{})
		<-ctx.Done()
		// No final message on an abrupt cancel — matches mcp.handleSubscriptionsListen.
	default:
		w.WriteMessage(NewSuccessResponse(req.ID, map[string]string{"echo": req.Method}))
	}
}

func readLineWithTimeout(t *testing.T, lines <-chan string, d time.Duration) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("output stream closed unexpectedly while waiting for a line")
		}
		return l
	case <-time.After(d):
		t.Fatal("timed out waiting for a line of output")
		return ""
	}
}

func expectNoLineWithin(t *testing.T, lines <-chan string, d time.Duration) {
	t.Helper()
	select {
	case l, ok := <-lines:
		if ok {
			t.Fatalf("expected no further output, got: %s", l)
		}
	case <-time.After(d):
	}
}

// TestStreamConcurrentDispatchAndCancellation exercises the property that makes the
// shared stream binding mandatory in a stateless protocol: a long-lived
// subscriptions/listen request must not block any other concurrent request on the same
// connection, and notifications/cancelled must stop further output for the request it
// names without disturbing anything else.
func TestStreamConcurrentDispatchAndCancellation(t *testing.T) {
	st := newStreamTransport("test")
	st.handler = streamTestHandler{}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		st.serve(ctx, inR, outW)
	}()

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	writeLine := func(s string) {
		if _, err := inW.Write([]byte(s + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Open a long-lived subscription first.
	writeLine(`{"jsonrpc":"2.0","id":"sub","method":"subscriptions/listen","params":{}}`)
	ack := readLineWithTimeout(t, lines, 2*time.Second)
	if !strings.Contains(ack, "acknowledged") {
		t.Fatalf("expected an acknowledgment first, got: %s", ack)
	}

	// While the subscription is still open, an unrelated request must complete without
	// waiting for it — this is the concurrency guarantee the stream binding exists for.
	writeLine(`{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{}}`)
	resp := readLineWithTimeout(t, lines, 2*time.Second)
	if !strings.Contains(resp, `"echo":"tools/call"`) {
		t.Fatalf("expected the concurrent tools/call to complete, got: %s", resp)
	}

	// Cancelling the subscription by id must stop its output without any further
	// message for it (in particular, no response to the still-open subscriptions/listen
	// request — an abrupt/cancelled subscription gets no final message).
	writeLine(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub"}}`)
	expectNoLineWithin(t, lines, 300*time.Millisecond)

	inW.Close()
	outW.Close()
	cancel()

	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after input closed and context was cancelled")
	}
}
