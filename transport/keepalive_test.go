package transport

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// lateWriteDetector stands in for net/http's response: once the handler has returned, any
// write is the bug (net/http has released the writer; a real one panics in bufio).
type lateWriteDetector struct {
	h        http.Header
	returned atomic.Bool
	late     atomic.Int32
}

func (d *lateWriteDetector) Header() http.Header { return d.h }
func (d *lateWriteDetector) WriteHeader(int)     {}
func (d *lateWriteDetector) Write(b []byte) (int, error) {
	if d.returned.Load() {
		d.late.Add(1)
	}
	return len(b), nil
}
func (d *lateWriteDetector) Flush() {
	if d.returned.Load() {
		d.late.Add(1)
	}
}

// The keep-alive goroutine must never write after the handler finished the response. Before
// finish() existed, a tick racing the handler's return wrote to a released writer and crashed
// the process.
func TestKeepAliveNeverWritesAfterFinish(t *testing.T) {
	for i := 0; i < 3000; i++ {
		d := &lateWriteDetector{h: http.Header{}}
		rw := newHTTPResponseWriter(d)
		rw.interval = 50 * time.Microsecond
		rw.startSSE()
		time.Sleep(time.Duration(i%7) * 20 * time.Microsecond)
		rw.finish()
		d.returned.Store(true) // the handler has returned
		if err := rw.WriteNotification("notifications/x", nil); err == nil {
			t.Fatal("a write after finish must be refused")
		}
		time.Sleep(100 * time.Microsecond)
		if n := d.late.Load(); n != 0 {
			t.Fatalf("round %d: %d writes after the handler returned", i, n)
		}
	}
}
