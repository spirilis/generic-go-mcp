package transport

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// newPipedStdio wires a StdioTransport to a pair of in-memory pipes and starts it, returning
// a writer for the transport's input, a channel of the lines it writes, and the transport
// itself. This is the seam NewStdioTransportWithStreams exists for: before it, the exported
// stdio type could not be exercised at all because it read os.Stdin directly.
func newPipedStdio(t *testing.T) (*StdioTransport, io.WriteCloser, <-chan string) {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	trans := NewStdioTransportWithStreams(inR, outW)
	if err := trans.Start(streamTestHandler{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	t.Cleanup(func() {
		trans.Stop()
		inW.Close()
		outW.Close()
	})

	return trans, inW, lines
}

func expectDoneWithin(t *testing.T, trans *StdioTransport, d time.Duration) {
	t.Helper()
	select {
	case <-trans.Done():
	case <-time.After(d):
		t.Fatal("Done() was not closed in time")
	}
}

// TestStdioTransportOverPipes is the baseline the hardcoded os.Stdin/os.Stdout used to make
// impossible: drive the exported transport end to end over injected streams.
func TestStdioTransportOverPipes(t *testing.T) {
	_, in, lines := newPipedStdio(t)

	if _, err := in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := readLineWithTimeout(t, lines, 2*time.Second)
	if !strings.Contains(resp, `"echo":"tools/call"`) {
		t.Fatalf("expected the request to be handled, got: %s", resp)
	}
}

// TestStdioTransportDoneOnEOF is the consumer's actual complaint: closing the input must be
// observable, so a stdio server can exit cleanly when its client goes away.
func TestStdioTransportDoneOnEOF(t *testing.T) {
	trans, in, _ := newPipedStdio(t)

	select {
	case <-trans.Done():
		t.Fatal("Done() closed while the input was still open")
	case <-time.After(50 * time.Millisecond):
	}

	in.Close()

	expectDoneWithin(t, trans, 2*time.Second)
	if err := trans.Err(); err != nil {
		t.Fatalf("expected a clean EOF to report no error, got: %v", err)
	}
}

// TestStdioTransportDoneBeforeStart guards the nil-channel trap: done is allocated by the
// constructor, so a caller that selects on Done() before Start simply waits, rather than
// selecting on a nil channel that can never fire.
func TestStdioTransportDoneBeforeStart(t *testing.T) {
	trans := NewStdioTransportWithStreams(strings.NewReader(""), io.Discard)

	if trans.Done() == nil {
		t.Fatal("Done() must be non-nil before Start")
	}
	select {
	case <-trans.Done():
		t.Fatal("Done() must not be closed before Start")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestStdioTransportStopBeforeStart covers the old nil-channel deadlock: Stop's unguarded
// receive from a nil done channel blocked forever.
func TestStdioTransportStopBeforeStart(t *testing.T) {
	trans := NewStdioTransportWithStreams(strings.NewReader(""), io.Discard)

	returned := make(chan error, 1)
	go func() { returned <- trans.Stop() }()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Stop before Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked when called before Start()")
	}
}

// TestStdioTransportStopWithInputOpen covers the hang that made Stop unusable: the read loop
// is parked in a blocking read that ctx cancellation cannot interrupt, so Stop must not wait
// on it. Nobody closes this pipe, which is exactly the signal-driven shutdown case.
func TestStdioTransportStopWithInputOpen(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()

	trans := NewStdioTransportWithStreams(inR, io.Discard)
	if err := trans.Start(streamTestHandler{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	returned := make(chan error, 1)
	go func() {
		trans.Stop()
		returned <- trans.Stop() // idempotent: a second Stop must not panic or block either
	}()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Stop with input open: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked while the input stream was still open")
	}
}

// TestStdioTransportErrOnReadError is what lets a consumer pick an exit code: a failed read
// must be distinguishable from a clean EOF.
func TestStdioTransportErrOnReadError(t *testing.T) {
	readErr := errors.New("simulated stdin failure")

	trans := NewStdioTransportWithStreams(iotest.ErrReader(readErr), io.Discard)
	if err := trans.Start(streamTestHandler{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	expectDoneWithin(t, trans, 2*time.Second)
	if !errors.Is(trans.Err(), readErr) {
		t.Fatalf("expected Err() to surface the read failure, got: %v", trans.Err())
	}
}

// TestStdioTransportDoubleStart: the shared stream binding is single-use, so a second Start
// must be refused rather than leaking a second read loop over the same streams.
func TestStdioTransportDoubleStart(t *testing.T) {
	trans := NewStdioTransportWithStreams(strings.NewReader(""), io.Discard)

	if err := trans.Start(streamTestHandler{}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := trans.Start(streamTestHandler{}); err == nil {
		t.Fatal("expected the second Start() to return an error")
	}
}

// TestStdioTransportConcurrentStartStop pins the absence of a data race between Start and a
// Stop racing it from another goroutine — the reason the transport's lifetime context is
// built by the constructor rather than by Start. Meaningful only under -race.
func TestStdioTransportConcurrentStartStop(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()

	trans := NewStdioTransportWithStreams(inR, io.Discard)

	go func() { trans.Stop() }()
	if err := trans.Start(streamTestHandler{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	trans.Stop()
}

// TestStdioTransportDefaultsToProcessStreams pins the documented nil-means-process-default
// behaviour, so NewStdioTransport and NewStdioTransportWithStreams(nil, nil) stay equivalent.
func TestStdioTransportDefaultsToProcessStreams(t *testing.T) {
	trans := NewStdioTransportWithStreams(nil, nil)
	if trans.in == nil || trans.out == nil {
		t.Fatal("nil streams must fall back to the process defaults, not stay nil")
	}
}
