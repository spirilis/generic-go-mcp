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
