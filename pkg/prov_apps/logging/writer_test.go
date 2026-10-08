package logging

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelWriter_CompleteLines(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	n, err := cw.Write([]byte("line1\nline2\n"))
	require.NoError(t, err)
	assert.Equal(t, 12, n)

	assert.Equal(t, "line1", <-ch)
	assert.Equal(t, "line2", <-ch)
}

func TestChannelWriter_PartialLines(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	_, _ = cw.Write([]byte("part"))
	_, _ = cw.Write([]byte("ial\n"))

	assert.Equal(t, "partial", <-ch)
}

func TestChannelWriter_WindowsLineEndings(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	_, _ = cw.Write([]byte("windows\r\n"))
	assert.Equal(t, "windows", <-ch)
}

func TestChannelWriter_FullChannel(t *testing.T) {
	ch := make(chan string, 1)
	cw := NewChannelWriter(ch)

	_, _ = cw.Write([]byte("msg1\nmsg2\nmsg3\n"))
	// msg1 should be in channel, msg2 and msg3 dropped
	assert.Equal(t, "msg1", <-ch)
}

func TestChannelWriter_Close(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	require.NoError(t, cw.Close())

	// Writes after close are silently ignored
	n, err := cw.Write([]byte("after close\n"))
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestChannelWriter_FlushPartialLine(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	_, _ = cw.Write([]byte("no newline"))
	cw.Flush()

	assert.Equal(t, "no newline", <-ch)
}

func TestChannelWriter_Concurrent(t *testing.T) {
	ch := make(chan string, 1000)
	cw := NewChannelWriter(ch)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			_, _ = cw.Write([]byte("concurrent line\n"))
		})
	}
	wg.Wait()
}

func TestMultiWriter_Write(t *testing.T) {
	ch1 := make(chan string, 10)
	ch2 := make(chan string, 10)
	cw1 := NewChannelWriter(ch1)
	cw2 := NewChannelWriter(ch2)

	mw := NewMultiWriter(cw1, cw2)
	n, err := mw.Write([]byte("hello\n"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	assert.Equal(t, "hello", <-ch1)
	assert.Equal(t, "hello", <-ch2)
}

func TestMultiWriter_NilDestinations(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)

	mw := NewMultiWriter(nil, cw, nil)
	assert.Equal(t, 1, mw.DestinationCount())

	n, err := mw.Write([]byte("test\n"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

func TestMultiWriter_Close(t *testing.T) {
	ch := make(chan string, 10)
	cw := NewChannelWriter(ch)
	mw := NewMultiWriter(cw)

	assert.NoError(t, mw.Close())
	assert.Equal(t, 0, mw.DestinationCount())
}

func TestMultiWriter_AddDestination(t *testing.T) {
	mw := NewMultiWriter()
	assert.Equal(t, 0, mw.DestinationCount())

	ch := make(chan string, 10)
	mw.AddDestination(NewChannelWriter(ch))
	assert.Equal(t, 1, mw.DestinationCount())

	mw.AddDestination(nil) // should be ignored
	assert.Equal(t, 1, mw.DestinationCount())
}
