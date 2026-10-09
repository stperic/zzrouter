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
//     The relay hands it whole lines, possibly several; it must preserve framing.
//
// A nil Registry and an unregistered provider are both valid — Body/Stream
// return nil, which callers treat as "no normalization." Hot paths stay
// allocation-free in the common case.
//
// The Registry is read-only after construction; no locking is needed.
// Concrete provider normalizers (e.g. MLX's reasoning→content shim in
// mlx.go) live in this package alongside the generic mechanism.
package normalizer

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
