// Package normalizer composes provider repairs and client usage cleanup before output.
package normalizer

import (
	"bytes"
	"encoding/json"

	"github.com/stperic/zzrouter/pkg/protocol"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
)

// BodyNormalizer rewrites a complete (non-streaming) response body.
// Returning nil or the input as-is signals "no change".
type BodyNormalizer func(body []byte) []byte

// StreamNormalizer rewrites a single chunk from a streaming response.
// Chunks may contain partial or multiple SSE data lines — implementations
// must preserve line framing. Returning the input unchanged is a no-op.
type StreamNormalizer func(chunk []byte) []byte

// ObjectNormalizer reports changes so no-op bodies retain their original bytes.
type ObjectNormalizer func(map[string]any) bool

// Registry is configured before use; editors share one decode per payload.
type Registry struct {
	edits map[string]ObjectNormalizer
	// CleanChatUsage is evaluated per response so worker role changes retain upstream evidence.
	CleanChatUsage func() bool
}

// NewRegistry leaves cleanup disabled until the client-facing hop enables it.
func NewRegistry() *Registry {
	return &Registry{edits: make(map[string]ObjectNormalizer)}
}

// Register installs an editor shared by the body and stream adapters.
func (r *Registry) Register(provider string, edit ObjectNormalizer) {
	if r != nil && edit != nil {
		r.edits[provider] = edit
	}
}

// Body preserves malformed bodies and no-op formatting, including numeric precision.
func (r *Registry) Body(provider string) BodyNormalizer {
	if r == nil {
		return nil
	}
	edit := r.edits[provider]
	clean := r.CleanChatUsage != nil && r.CleanChatUsage()
	if edit == nil && !clean {
		return nil
	}
	return func(body []byte) []byte {
		if !(clean && (bytes.Contains(body, []byte("cost")) || bytes.Contains(body, []byte("is_byok")) || bytes.Contains(body, []byte("cache_write_tokens")))) &&
			!(edit != nil && bytes.Contains(body, []byte("reasoning"))) {
			return body
		}
		var obj map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if decoder.Decode(&obj) != nil || len(bytes.TrimSpace(body[decoder.InputOffset():])) != 0 {
			return body
		}
		changed := false
		if clean {
			changed = openai.NormalizeChatUsage(obj)
		}
		if edit != nil {
			changed = edit(obj) || changed
		}
		if !changed {
			return body
		}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
}

// Stream shares the body editor so repairs and cleanup cannot diverge across transports.
func (r *Registry) Stream(provider string) StreamNormalizer {
	body := r.Body(provider)
	if body == nil {
		return nil
	}
	return func(chunk []byte) []byte {
		return protocol.RewriteStreamJSON(chunk, body)
	}
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
