package normalizer

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// singleShotReader returns its buffered data + err in ONE Read call
// (bytes + terminal error together), then returns (0, err) on any
// further Read. Models a backend HTTP body that returns the whole
// frame and EOF in a single read — common for small responses.
type singleShotReader struct {
	data []byte
	err  error
	done bool
}

func (s *singleShotReader) Read(p []byte) (int, error) {
	if s.done {
		return 0, s.err
	}
	s.done = true
	n := copy(p, s.data)
	return n, s.err
}

func (s *singleShotReader) Close() error { return nil }

func TestComposeStream(t *testing.T) {
	upper := StreamNormalizer(func(b []byte) []byte {
		return []byte(strings.ToUpper(string(b)))
	})
	exclaim := func(b []byte) []byte {
		return append(b, '!')
	}

	if ComposeStream(nil, nil) != nil {
		t.Error("both nil should produce nil transform")
	}
	single := ComposeStream(upper, nil)
	if got := string(single([]byte("abc"))); got != "ABC" {
		t.Errorf("upper-only: got %q", got)
	}
	chained := ComposeStream(upper, exclaim)
	if got := string(chained([]byte("abc"))); got != "ABC!" {
		t.Errorf("chain: got %q", got)
	}
}

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	r.RegisterBody("mlx", func(b []byte) []byte { return b })
	r.RegisterStream("mlx", func(b []byte) []byte { return b })

	if r.Body("mlx") == nil {
		t.Error("mlx body normalizer not registered")
	}
	if r.Stream("mlx") == nil {
		t.Error("mlx stream normalizer not registered")
	}
	if r.Body("ollama") != nil {
		t.Error("ollama should have no body normalizer")
	}
	if r.Stream("ollama") != nil {
		t.Error("ollama should have no stream normalizer")
	}

	// nil-receiver safety
	var nilReg *Registry
	if nilReg.Body("mlx") != nil || nilReg.Stream("mlx") != nil {
		t.Error("nil receiver lookups should return nil")
	}

	// nil-fn register is a no-op (does not panic).
	r.RegisterBody("nope", nil)
	r.RegisterStream("nope", nil)
	if r.Body("nope") != nil || r.Stream("nope") != nil {
		t.Error("nil fn should not be registered")
	}
}

// TestNewStreamReader_ExpansionPlusEOFDoesNotDropBytes exercises the
// io.Reader contract invariant: when the normalizer expands output
// beyond the caller's buffer AND upstream returned bytes + io.EOF in
// the same Read, the stashed remainder MUST be delivered before the
// EOF is surfaced. Returning (n, EOF) with bytes still in buf violates
// io.Reader — callers are entitled to stop on EOF, so the stashed
// bytes would be silently lost.
func TestNewStreamReader_ExpansionPlusEOFDoesNotDropBytes(t *testing.T) {
	// Normalizer doubles the input ("hello" → "hellohello").
	double := StreamNormalizer(func(b []byte) []byte {
		out := make([]byte, 2*len(b))
		copy(out, b)
		copy(out[len(b):], b)
		return out
	})

	// Upstream returns 5 bytes + EOF in one Read.
	src := &singleShotReader{data: []byte("hello"), err: io.EOF}
	r := NewStreamReader(src, double)

	// Caller buffer is 5 bytes — smaller than doubled output (10).
	// First Read: must deliver 5 bytes WITHOUT EOF (buf still has 5
	// bytes left).
	buf1 := make([]byte, 5)
	n1, err1 := r.Read(buf1)
	require.Equal(t, 5, n1)
	require.NoError(t, err1,
		"first Read must not return EOF while buffer still holds normalized remainder")
	assert.Equal(t, "hello", string(buf1))

	// Second Read: drain the stashed remainder + surface EOF.
	buf2 := make([]byte, 10)
	n2, err2 := r.Read(buf2)
	require.Equal(t, 5, n2)
	assert.Equal(t, io.EOF, err2, "second Read must surface EOF alongside remaining bytes")
	assert.Equal(t, "hello", string(buf2[:n2]))

	// Third Read: no more data, EOF already delivered once.
	// singleShotReader returns (0, io.EOF) again; normalizingReader
	// passes it through.
	buf3 := make([]byte, 10)
	n3, err3 := r.Read(buf3)
	assert.Equal(t, 0, n3)
	assert.Equal(t, io.EOF, err3)
}

// TestNewStreamReader_NonExpandingPreservesEOFCoalescing verifies that
// when the normalizer does NOT expand the chunk, the (bytes, EOF) pair
// is forwarded as-is in a single Read — preserving the original
// coalescing behaviour that efficient callers (io.Copy, io.ReadAll)
// depend on.
func TestNewStreamReader_NonExpandingPreservesEOFCoalescing(t *testing.T) {
	identity := StreamNormalizer(func(b []byte) []byte { return b })
	src := &singleShotReader{data: []byte("hello"), err: io.EOF}
	r := NewStreamReader(src, identity)

	buf := make([]byte, 32)
	n, err := r.Read(buf)
	assert.Equal(t, 5, n)
	assert.Equal(t, io.EOF, err)
	assert.Equal(t, "hello", string(buf[:n]))
}
