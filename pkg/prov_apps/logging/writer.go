package logging

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// ChannelWriter adapts a string channel to io.Writer.
// Buffers partial lines and sends complete lines to the channel.
// Thread-safe. Non-blocking: drops messages if the channel is full.
type ChannelWriter struct {
	ch     chan string
	buffer *bytes.Buffer
	mu     sync.Mutex
	closed bool
}

// NewChannelWriter creates a writer that sends lines to a channel.
func NewChannelWriter(ch chan string) *ChannelWriter {
	return &ChannelWriter{
		ch:     ch,
		buffer: &bytes.Buffer{},
	}
}

// Write implements io.Writer. Buffers data and sends complete lines.
func (cw *ChannelWriter) Write(p []byte) (n int, err error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.closed {
		return 0, nil
	}

	written, err := cw.buffer.Write(p)
	if err != nil {
		return written, err
	}

	cw.flushLinesLocked()
	return written, nil
}

func (cw *ChannelWriter) flushLinesLocked() {
	for {
		line, err := cw.buffer.ReadString('\n')
		if err != nil {
			if line != "" {
				newBuf := bytes.NewBuffer([]byte(line))
				newBuf.Write(cw.buffer.Bytes())
				cw.buffer = newBuf
			}
			break
		}
		// Trim newline/carriage return
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		cw.trySend(line)
	}
}

func (cw *ChannelWriter) trySend(msg string) {
	if cw.ch == nil {
		return
	}
	select {
	case cw.ch <- msg:
	default:
	}
}

// Flush sends any remaining buffered data.
func (cw *ChannelWriter) Flush() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.closed || cw.buffer.Len() == 0 {
		return nil
	}

	msg := cw.buffer.String()
	msg = trimTrailingNewlines(msg)
	cw.trySend(msg)
	cw.buffer.Reset()
	return nil
}

// Close flushes and marks the writer as closed. Writes after Close are no-ops.
func (cw *ChannelWriter) Close() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.closed {
		return nil
	}

	if cw.buffer.Len() > 0 {
		msg := trimTrailingNewlines(cw.buffer.String())
		cw.trySend(msg)
		cw.buffer.Reset()
	}

	cw.closed = true
	return nil
}

func trimTrailingNewlines(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}

// MultiWriter writes to multiple io.Writer destinations simultaneously.
// Thread-safe. If one destination fails, writing continues to others.
type MultiWriter struct {
	destinations []io.Writer
	mu           sync.RWMutex
}

// NewMultiWriter creates a MultiWriter from the given destinations.
// Nil destinations are filtered out.
func NewMultiWriter(destinations ...io.Writer) *MultiWriter {
	valid := make([]io.Writer, 0, len(destinations))
	for _, d := range destinations {
		if d != nil {
			valid = append(valid, d)
		}
	}
	return &MultiWriter{destinations: valid}
}

// Write writes to all destinations.
func (mw *MultiWriter) Write(p []byte) (n int, err error) {
	mw.mu.RLock()
	defer mw.mu.RUnlock()

	if len(mw.destinations) == 0 {
		return 0, nil
	}

	minWritten := len(p)
	var firstErr error
	failedCount := 0

	for i, dest := range mw.destinations {
		written, writeErr := dest.Write(p)
		if writeErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("destination %d: %w", i, writeErr)
			}
			failedCount++
			continue
		}
		if written < minWritten {
			minWritten = written
		}
	}

	if failedCount == len(mw.destinations) {
		return 0, fmt.Errorf("all destinations failed: %w", firstErr)
	}
	if firstErr != nil {
		return minWritten, firstErr
	}
	return minWritten, nil
}

// AddDestination adds a writer destination.
func (mw *MultiWriter) AddDestination(dest io.Writer) {
	if dest == nil {
		return
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	mw.destinations = append(mw.destinations, dest)
}

// DestinationCount returns the number of active destinations.
func (mw *MultiWriter) DestinationCount() int {
	mw.mu.RLock()
	defer mw.mu.RUnlock()
	return len(mw.destinations)
}

// Close closes all destinations that implement io.Closer.
func (mw *MultiWriter) Close() error {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	var errs []error
	for i, dest := range mw.destinations {
		if closer, ok := dest.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("destination %d: %w", i, err))
			}
		}
	}
	mw.destinations = nil

	if len(errs) > 0 {
		return fmt.Errorf("failed to close %d destinations", len(errs))
	}
	return nil
}
