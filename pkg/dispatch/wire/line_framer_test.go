package wire

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pieceReader hands back exactly one predefined piece per Read, which is
// what a network body does and what strings.Reader never does: a
// 32 KiB buffer swallows any test fixture whole, so a frame boundary can
// only be forced by controlling the reads.
type pieceReader struct {
	pieces []string
	closes int
}

func (p *pieceReader) Read(b []byte) (int, error) {
	if len(p.pieces) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.pieces[0])
	if rest := p.pieces[0][n:]; rest == "" {
		p.pieces = p.pieces[1:]
	} else {
		p.pieces[0] = rest
	}
	return n, nil
}

func (p *pieceReader) Close() error { p.closes++; return nil }

// splitEvery cuts s into pieces of at most n bytes, the crude version of
// what TCP does to an SSE stream.
func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// The defect this type exists for: a line delivered in two reads must
// come back out as one line, not two fragments.
func TestLineFramerRejoinsSplitLine(t *testing.T) {
	var f lineFramer

	assert.Nil(t, f.next([]byte(`data: {"usage":{"prompt`)),
		"a line with no newline yet is not ready to hand on")
	assert.Equal(t, `data: {"usage":{"prompt_tokens":11}}`+"\n",
		string(f.next([]byte(`_tokens":11}}`+"\n"))),
		"the newline completes the held line and releases it whole")
}

// Whole lines go on immediately; only the unterminated tail waits.
func TestLineFramerReleasesWholeLinesAndHoldsTheTail(t *testing.T) {
	var f lineFramer

	assert.Equal(t, "a\nb\n", string(f.next([]byte("a\nb\nc"))),
		"complete lines must not be delayed by an incomplete one behind them")
	assert.Equal(t, "c", string(f.flush()), "flush releases the held tail")
	assert.Nil(t, f.flush(), "flush is idempotent")
}

// An upstream that ends its last frame without a newline still has to be
// observed: at EOF that frame is as complete as it will ever be.
func TestLineFramerFlushReleasesUnterminatedFinalLine(t *testing.T) {
	var f lineFramer

	assert.Nil(t, f.next([]byte(`{"done":true}`)))
	assert.Equal(t, `{"done":true}`, string(f.flush()))
}

// Framing must be transparent: whatever the read sizes, the bytes out
// equal the bytes in, in order.
func TestLineFramerPreservesTheStream(t *testing.T) {
	const stream = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	for _, size := range []int{1, 3, 7, 16, 64, len(stream)} {
		var f lineFramer
		var got strings.Builder
		for _, piece := range splitEvery(stream, size) {
			got.Write(f.next([]byte(piece)))
		}
		got.Write(f.flush())
		assert.Equal(t, stream, got.String(), "read size %d must not change the stream", size)
	}
}

// A stream that is not line-delimited at all must not be buffered
// without bound; framing degrades to passing bytes through.
func TestLineFramerStopsHoldingPastTheCap(t *testing.T) {
	var f lineFramer
	blob := strings.Repeat("x", maxPendingLine/2+1)

	require.Nil(t, f.next([]byte(blob)), "first half fits under the cap and is held")
	assert.Equal(t, 2*len(blob), len(f.next([]byte(blob))),
		"crossing the cap releases everything held plus the current read")
	assert.Nil(t, f.flush(), "nothing stays held after an overflow release")
}
