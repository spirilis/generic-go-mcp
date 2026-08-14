package transport

import "sync"

// BufferedNotification records one WriteNotification call captured by a
// BufferedResponseWriter.
type BufferedNotification struct {
	Method string
	Params interface{}
}

// BufferedResponseWriter is a ResponseWriter that buffers every notification and the
// final message in memory, rather than writing to a live connection. It's for callers
// that invoke a MessageHandler directly — bypassing a Transport entirely — and don't need
// streaming: tests, or a synchronous embedding of Server.HandleMessage in a program that
// already has its own request/response model.
//
// It is safe for concurrent use, since a long-lived request (subscriptions/listen) may
// call WriteNotification from the same goroutine that's still being awaited by another
// goroutine reading Notifications/Message.
type BufferedResponseWriter struct {
	mu            sync.Mutex
	notifications []BufferedNotification
	message       []byte
	headers       map[string]string
}

// NewBufferedResponseWriter creates an empty BufferedResponseWriter.
func NewBufferedResponseWriter() *BufferedResponseWriter {
	return &BufferedResponseWriter{}
}

// WriteNotification implements ResponseWriter.
func (w *BufferedResponseWriter) WriteNotification(method string, params interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notifications = append(w.notifications, BufferedNotification{Method: method, Params: params})
	return nil
}

// WriteMessage implements ResponseWriter.
func (w *BufferedResponseWriter) WriteMessage(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.message = data
	return nil
}

// Notifications returns a snapshot of every notification written so far.
func (w *BufferedResponseWriter) Notifications() []BufferedNotification {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]BufferedNotification, len(w.notifications))
	copy(out, w.notifications)
	return out
}

// Message returns the final JSON-RPC response written so far, or nil if HandleMessage
// hasn't called WriteMessage yet (e.g. it's a notification, or a still-open
// subscriptions/listen request).
func (w *BufferedResponseWriter) Message() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.message
}

// SetResponseHeader implements HeaderSetter, so tests exercising a legacy-compatibility
// layer directly (bypassing a live HTTP connection) can observe headers such as a minted
// Mcp-Session-Id. Like a real HTTP response, a header set after WriteMessage has already
// been called is silently dropped.
func (w *BufferedResponseWriter) SetResponseHeader(name, value string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.message != nil {
		return
	}
	if w.headers == nil {
		w.headers = make(map[string]string)
	}
	w.headers[name] = value
}

// ResponseHeaders returns a snapshot of every header set so far via SetResponseHeader.
func (w *BufferedResponseWriter) ResponseHeaders() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]string, len(w.headers))
	for k, v := range w.headers {
		out[k] = v
	}
	return out
}

var _ HeaderSetter = (*BufferedResponseWriter)(nil)
