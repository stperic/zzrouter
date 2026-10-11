package wire

import (
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// StreamTransform is the chunk chain every relayed stream goes through: the provider's normalizer,
// the zzrouter usage block, the caller's model name, then the usage-only-frame strip.
type StreamTransform struct {
	fn        func([]byte) []byte
	usageSeen bool
}

// NewStreamTransform builds the chain. Strip runs last, which is safe only because callers meter from
// the raw bytes before any transform.
func NewStreamTransform(normalize normalizer.StreamNormalizer, meta RoutingMetadata, recorder *llm.InferenceRecorder) *StreamTransform {
	t := &StreamTransform{}

	var inject func([]byte) []byte
	if meta.InjectUsage {
		inject = func(chunk []byte) []byte {
			// Test for a usage object, not the substring: `"usage":null` rides on every delta frame.
			if ChunkHasUsageObject(chunk) {
				t.usageSeen = true
			}
			return InjectUsageIntoSSEChunk(chunk, MetadataWithSnapshot(meta, recorder))
		}
	}

	var restore func([]byte) []byte
	if meta.ClientModel != "" {
		restore = func(chunk []byte) []byte {
			return RewriteModelInStreamChunk(chunk, meta.ClientModel)
		}
	}

	var strip func([]byte) []byte
	if meta.SuppressUsageFrame {
		strip = StripUsageOnlyFrames
	}

	t.fn = normalizer.ComposeStream(normalize, normalizer.ChainChunks(inject, restore, strip))
	return t
}

// Func returns the composed chunk function, or nil when no stage applies.
func (t *StreamTransform) Func() func([]byte) []byte {
	return t.fn
}

// UsageSeen reports whether a chunk carrying a real usage object passed through.
func (t *StreamTransform) UsageSeen() bool {
	return t.usageSeen
}
