// Package normalizer provides per-provider response-body + stream
// rewriters used by zzRouter's proxy copy sites.
//
// Problem: different LLM providers return OpenAI-compatible responses with
// minor deviations (e.g. MLX's mlx_lm server returns delta.reasoning instead
// of delta.content). OpenAI SDK clients only read delta.content and the chat
// appears empty.
//
// Design: per-provider normalizers are registered on a Registry at startup
// and applied at each proxy copy site. Two kinds of normalizer exist:
//
//   - BodyNormalizer: rewrites a buffered, non-streaming JSON response.
//   - StreamNormalizer: rewrites a raw SSE/NDJSON chunk as it passes through.
//     Called once per read from the backend and must preserve framing —
//     chunks may contain zero, one, or many SSE data lines.
//
// A nil Registry and an unregistered provider are both valid — Body/Stream
// return nil, which callers treat as "no normalization." Hot paths stay
// allocation-free in the common case.
//
// The Registry is read-only after construction; no locking is needed.
// Concrete provider normalizers (e.g. MLX's reasoning→content shim in
// mlx.go) live in this package alongside the generic mechanism.
package normalizer

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
)

// BodyNormalizer rewrites a complete (non-streaming) response body.
// Returning nil or the input as-is signals "no change".
type BodyNormalizer func(body []byte) []byte

// StreamNormalizer rewrites a single chunk from a streaming response.
// Chunks may contain partial or multiple SSE data lines — implementations
// must preserve line framing. Returning the input unchanged is a no-op.
type StreamNormalizer func(chunk []byte) []byte

// Registry holds per-provider response transforms. The zero value is NOT
// usable — use NewRegistry. Lookups are read-only after construction so no
// locking is needed.
type Registry struct {
	body   map[string]BodyNormalizer
	stream map[string]StreamNormalizer
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		body:   make(map[string]BodyNormalizer),
		stream: make(map[string]StreamNormalizer),
	}
}

// RegisterBody adds a body normalizer for a provider key.
func (r *Registry) RegisterBody(provider string, fn BodyNormalizer) {
	if r == nil || fn == nil {
		return
	}
	r.body[provider] = fn
}

// RegisterStream adds a stream normalizer for a provider key.
func (r *Registry) RegisterStream(provider string, fn StreamNormalizer) {
	if r == nil || fn == nil {
		return
	}
	r.stream[provider] = fn
}

// Body returns the body normalizer for a provider, or nil if none is
// registered. Nil-receiver safe.
func (r *Registry) Body(provider string) BodyNormalizer {
	if r == nil {
		return nil
	}
	return r.body[provider]
}

// Stream returns the stream normalizer for a provider, or nil if none is
// registered. Nil-receiver safe.
func (r *Registry) Stream(provider string) StreamNormalizer {
	if r == nil {
		return nil
	}
	return r.stream[provider]
}

// ApplyBody reads resp.Body, runs the normalizer, replaces the body with
// the rewritten bytes, and updates Content-Length. Used for non-streaming
// responses in the reverse-proxy ModifyResponse hook.
func ApplyBody(resp *http.Response, fn BodyNormalizer) error {
	if resp == nil || fn == nil || resp.Body == nil {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	rewritten := fn(body)
	if rewritten == nil {
		rewritten = body
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

// normalizingReader wraps an io.ReadCloser and applies a StreamNormalizer
// to each chunk as it is read. Framing is preserved at the chunk boundary:
// the normalizer receives whatever bytes the underlying reader produced in
// a single Read, matching the semantics callers already rely on for SSE.
//
// This is used by the reverse proxy's ModifyResponse hook where we can't
// easily substitute the direct copy path. For direct call sites that
// already use a copy helper, compose transforms via ComposeStream instead
// of wrapping here.
//
// Contract: when the normalizer expands a chunk beyond the caller's buffer
// AND upstream returned its terminal error in the same Read, the stashed
// remainder is drained across subsequent Read calls; the terminal error
// (e.g. io.EOF) is held back until the buffer is empty. Returning
// (n, io.EOF) with bytes still stashed would violate io.Reader: once
// EOF is returned, callers are entitled to stop, and the stashed bytes
// would be silently lost.
type normalizingReader struct {
	r  io.ReadCloser
	fn StreamNormalizer
	// buf holds any residue from a previous chunk that could not fit into
	// the caller's p buffer during the last Read. It is drained on the
	// next Read before we fetch more bytes from r.
	buf bytes.Buffer
	// readErr holds an error returned by r alongside bytes that were
	// stashed into buf — it is surfaced to the caller only once buf is
	// fully drained.
	readErr error
}

// NewStreamReader returns an io.ReadCloser that applies fn to every chunk
// read from r, preserving framing at the chunk boundary.
func NewStreamReader(r io.ReadCloser, fn StreamNormalizer) io.ReadCloser {
	return &normalizingReader{r: r, fn: fn}
}

func (n *normalizingReader) Read(p []byte) (int, error) {
	// Drain leftover normalized bytes first. Surface a pending readErr
	// only when the buffer is empty — never return (n > 0, EOF) while
	// there are still bytes to deliver.
	if n.buf.Len() > 0 {
		rn, _ := n.buf.Read(p)
		if n.buf.Len() == 0 && n.readErr != nil {
			err := n.readErr
			n.readErr = nil
			return rn, err
		}
		return rn, nil
	}

	// Buffer empty; if we stashed an error from the previous upstream
	// Read, deliver it now without touching r again.
	if n.readErr != nil {
		err := n.readErr
		n.readErr = nil
		return 0, err
	}

	raw := make([]byte, len(p))
	rn, err := n.r.Read(raw)
	if rn > 0 {
		rewritten := n.fn(raw[:rn])
		if rewritten == nil {
			rewritten = raw[:rn]
		}
		if len(rewritten) <= len(p) {
			copy(p, rewritten)
			return len(rewritten), err
		}
		// Rewritten output is longer than the caller's buffer — copy what
		// fits, stash the rest AND the upstream error. The error is
		// returned on a subsequent Read once the buffer drains.
		copy(p, rewritten[:len(p)])
		n.buf.Write(rewritten[len(p):])
		n.readErr = err
		return len(p), nil
	}
	return 0, err
}

func (n *normalizingReader) Close() error {
	return n.r.Close()
}

// ChainChunks runs chunk transforms in order, skipping nils. Separate from
// ComposeStream because that one takes a StreamNormalizer first; this
// composes peers.
//
// Order is the caller's contract, not an implementation detail: a transform
// that inspects a frame has to run before one that may drop it.
func ChainChunks(transforms ...func([]byte) []byte) func([]byte) []byte {
	live := make([]func([]byte) []byte, 0, len(transforms))
	for _, t := range transforms {
		if t != nil {
			live = append(live, t)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	return func(chunk []byte) []byte {
		for _, t := range live {
			chunk = t(chunk)
		}
		return chunk
	}
}

// ComposeStream combines a provider-level StreamNormalizer with an in-path
// byte transform (e.g. the usage-metadata injector used by the proxy copy
// sites) into a single chain. Either argument may be nil. Returns nil when
// both are nil so callers can check `if transform != nil`.
func ComposeStream(normalize StreamNormalizer, inner func([]byte) []byte) func([]byte) []byte {
	switch {
	case normalize == nil && inner == nil:
		return nil
	case normalize == nil:
		return inner
	case inner == nil:
		return func(chunk []byte) []byte { return normalize(chunk) }
	default:
		return func(chunk []byte) []byte {
			return inner(normalize(chunk))
		}
	}
}
