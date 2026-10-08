package utils

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCapped_UnderMax_ReturnsFull(t *testing.T) {
	t.Parallel()
	data := []byte("hello world")
	got, err := ReadCapped(bytes.NewReader(data), 1024)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestReadCapped_ExactlyMax_ReturnsFull(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("a"), 1024)
	got, err := ReadCapped(bytes.NewReader(data), 1024)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestReadCapped_OverMax_ReturnsError(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("a"), 1025)
	_, err := ReadCapped(bytes.NewReader(data), 1024)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
}

func TestReadCapped_WayOverMax_ReturnsError(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB
	_, err := ReadCapped(bytes.NewReader(data), 1024)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
}

func TestReadCapped_Empty(t *testing.T) {
	t.Parallel()
	got, err := ReadCapped(bytes.NewReader(nil), 1024)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestReadCapped_ZeroMax_Errors(t *testing.T) {
	t.Parallel()
	_, err := ReadCapped(strings.NewReader("x"), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max must be > 0")
}

func TestReadCapped_NegativeMax_Errors(t *testing.T) {
	t.Parallel()
	_, err := ReadCapped(strings.NewReader("x"), -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max must be > 0")
}

type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

func TestReadCapped_PropagatesReaderErr(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	_, err := ReadCapped(errReader{err: want}, 1024)
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

// Pin: ErrResponseTooLarge wraps cleanly for errors.Is so downstream
// call sites (cluster dispatch, routing) can branch on it without
// string matching.
func TestReadCapped_ErrorIsMatchable(t *testing.T) {
	t.Parallel()
	_, err := ReadCapped(bytes.NewReader(bytes.Repeat([]byte{1}, 100)), 10)
	require.Error(t, err)
	target := ErrResponseTooLarge
	assert.True(t, errors.Is(err, target),
		"ReadCapped's oversize error must satisfy errors.Is(err, ErrResponseTooLarge)")
}

// Verify the io.Reader contract: ReadCapped consumes at most max+1
// bytes from the reader even for oversized inputs. This matters for
// HTTP response bodies: Close() should follow naturally without having
// drained megabytes of slack.
func TestReadCapped_DoesNotReadBeyondCap(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("x"), 1<<20)
	r := &countingReader{Reader: bytes.NewReader(data)}

	_, err := ReadCapped(r, 1024)
	require.Error(t, err)
	// +1 for the oversize detection byte. Reader may overshoot by a
	// small constant because io.ReadAll uses buffered reads, but it
	// should NOT slurp the full 1 MiB.
	assert.Less(t, r.bytesRead, int64(64*1024),
		"ReadCapped must stop after max+1 bytes + buffer slack, got %d", r.bytesRead)
}

type countingReader struct {
	io.Reader
	bytesRead int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.bytesRead += int64(n)
	return n, err
}
