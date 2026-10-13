package jobs

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadSSEOnlyFlushesCompleteFrames(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		frames     int
		tooLarge   bool
	}{
		{"clean trailing EOF", "event: done\ndata: terminal", 1, false},
		{"comments and multiline data", ":ping\n\nevent: progress\ndata: first\ndata: second\n\n", 1, false},
		{"oversized buffered continuation", "event: done\ndata: terminal\ndata: " + strings.Repeat("x", 1024*1024+1), 0, true},
		{"oversized multiline frame", "data: " + strings.Repeat("x", 600000) + "\ndata: " + strings.Repeat("y", 600000) + "\n\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := 0
			err := ReadSSE(strings.NewReader(tc.wire), func(string, []byte) (bool, error) { frames++; return false, nil })
			if tc.tooLarge {
				require.ErrorIs(t, err, bufio.ErrTooLong)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.frames, frames)
		})
	}
}

type interruptedReader struct{ wire string }

func (r *interruptedReader) Read(p []byte) (int, error) {
	if r.wire == "" {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.wire)
	r.wire = r.wire[n:]
	return n, nil
}
func TestReadSSETransportErrorDoesNotDispatchPartialTerminal(t *testing.T) {
	seen := false
	err := ReadSSE(&interruptedReader{wire: "event: done\ndata: terminal"}, func(string, []byte) (bool, error) { seen = true; return true, nil })
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	require.False(t, seen)
}
