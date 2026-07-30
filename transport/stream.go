package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/spirilis/generic-go-mcp/logging"
)

// streamTransport implements the newline-delimited JSON-RPC framing shared by every
// reliable-byte-stream binding (stdio, UNIX domain sockets, or any similar channel). Per
// the 2026-07-28 stdio transport spec, custom transports over such a stream SHOULD reuse
// this framing rather than defining a new one — only process/connection lifecycle is
// binding-specific, which is why stdio.go and unix.go are thin wrappers around this type.
//
// Because the protocol is stateless and subscriptions/listen never returns on its own,
// messages MUST be dispatched concurrently: a single serial read-dispatch-write loop
// would deadlock the moment one request opens a long-lived subscription. Each incoming
// line is therefore handled in its own goroutine; the single shared writer is
// serialized with a mutex so concurrent notifications/responses don't interleave
// mid-line.
type streamTransport struct {
	name string // for logging, e.g. "stdio" or "unix"

	handler MessageHandler

	writeMu sync.Mutex
	out     io.Writer

	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc // JSON-RPC id (as string) -> cancel

	wg sync.WaitGroup
}

func newStreamTransport(name string) *streamTransport {
	return &streamTransport{
		name:     name,
		inflight: make(map[string]context.CancelFunc),
	}
}

// serve runs the read loop against r, writing responses/notifications to w, until r
// returns EOF/error or ctx is cancelled. It blocks until all in-flight handlers have
// returned. Safe to call once per connection.
func (s *streamTransport) serve(ctx context.Context, r io.Reader, w io.Writer) {
	s.out = w

	scanner := bufio.NewScanner(r)
	// Allow generously large single-line messages (default bufio max is 64KiB, which is
	// easy to exceed with embedded resources/images).
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg := make([]byte, len(line))
		copy(msg, line)

		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		default:
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			logging.Debug("JSON-RPC parse error", "transport", s.name, "error", err)
			s.writeLine(NewErrorResponse(nil, &RPCError{Code: ParseError, Message: "Parse error"}))
			continue
		}

		if req.Method == "notifications/cancelled" {
			s.handleCancelled(req.Params)
			continue
		}

		reqCtx := ctx
		var cancel context.CancelFunc
		idKey := string(req.ID)
		if !req.IsNotification() {
			reqCtx, cancel = context.WithCancel(ctx)
			s.inflightMu.Lock()
			s.inflight[idKey] = cancel
			s.inflightMu.Unlock()
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if cancel != nil {
				defer func() {
					cancel()
					s.inflightMu.Lock()
					delete(s.inflight, idKey)
					s.inflightMu.Unlock()
				}()
			}
			s.handler.HandleMessage(reqCtx, msg, &streamResponseWriter{t: s})
		}()
	}

	if err := scanner.Err(); err != nil {
		logging.Debug("stream scanner error", "transport", s.name, "error", err)
	}
	s.wg.Wait()
}

// handleCancelled cancels the in-flight request named by the notification's params.requestId.
func (s *streamTransport) handleCancelled(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	key := string(p.RequestID)
	s.inflightMu.Lock()
	cancel, ok := s.inflight[key]
	s.inflightMu.Unlock()
	if ok {
		cancel()
	}
}

func (s *streamTransport) writeLine(data []byte) error {
	if data == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(data); err != nil {
		return err
	}
	_, err := s.out.Write([]byte("\n"))
	return err
}

// streamResponseWriter implements ResponseWriter by writing each message as its own line
// on the shared stream, serialized against every other concurrent request's output.
type streamResponseWriter struct {
	t *streamTransport
}

func (w *streamResponseWriter) WriteNotification(method string, params interface{}) error {
	data, err := json.Marshal(struct {
		JSONRPC string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return w.t.writeLine(data)
}

func (w *streamResponseWriter) WriteMessage(data []byte) error {
	return w.t.writeLine(data)
}
