package transport

import (
	"bufio"
	"fmt"
	"os"
	"sync"
)

// StdioTransport implements Transport using stdin/stdout
type StdioTransport struct {
	scanner *bufio.Scanner
	handler MessageHandler
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewStdioTransport creates a new stdio transport
func NewStdioTransport() *StdioTransport {
	return &StdioTransport{
		scanner: bufio.NewScanner(os.Stdin),
		stopCh:  make(chan struct{}),
	}
}

// Start begins reading from stdin and processing messages
func (t *StdioTransport) Start(handler MessageHandler) error {
	t.handler = handler

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.readLoop()
	}()

	return nil
}

// Stop gracefully stops the transport
func (t *StdioTransport) Stop() error {
	close(t.stopCh)
	t.wg.Wait()
	return nil
}

// readLoop continuously reads lines from stdin
func (t *StdioTransport) readLoop() {
	for {
		select {
		case <-t.stopCh:
			return
		default:
			if t.scanner.Scan() {
				line := t.scanner.Bytes()
				if len(line) == 0 {
					continue
				}

				// Handle the message
				response := t.handler.HandleMessage(line)

				// Write response to stdout
				if response != nil {
					fmt.Println(string(response))
				}
			} else {
				// Check for scanner error or EOF
				if err := t.scanner.Err(); err != nil {
					fmt.Fprintf(os.Stderr, "Scanner error: %v\n", err)
				}
				return
			}
		}
	}
}
