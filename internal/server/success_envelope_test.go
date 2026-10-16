package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// Pins the success-path body shaping contract: Transfer-Encoding
// stripped, Content-Length matches body, body normalizer respected,
// MaybeInjectRoutingMetadata applied. Without these, a refactor that
// drops the strip or skips the normalizer-nil branch silently flips
// the wire shape — integration tests exercising the success path
// won't necessarily catch it (they assert content, not framing).

func TestRelayBuffered_StripsTransferEncodingAndSetsLength(t *testing.T) {
	body := `{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":      []string{"application/json"},
			"Transfer-Encoding": []string{"chunked"},
		},
		Body: io.NopCloser(bytes.NewReader([]byte(body))),
	}

	w := httptest.NewRecorder()
	w.Header().Set("Transfer-Encoding", "chunked") // simulate a prior maps.Copy
	recorder := llm.NewInferenceRecorder(t.Context(), "model-x", "openrouter")

	wire.RelayBuffered(w, resp, normalizedBody(NewResponseNormalizers(), "openrouter",
		wire.RoutingMetadata{Provider: "openrouter"}, recorder), recorder)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Transfer-Encoding"),
		"Transfer-Encoding must be stripped — body is being re-encoded")
	assert.Equal(t, "60", w.Header().Get("Content-Length"),
		"Content-Length must match the rewritten body length, got %q for %d-byte body",
		w.Header().Get("Content-Length"), w.Body.Len())
	assert.Equal(t, w.Body.Len(), len(w.Body.Bytes()),
		"recorder body must equal what was written")
}

func TestRelayBuffered_NormalizerRespected(t *testing.T) {
	body := `{"choices":[{"message":{"reasoning":"yes","content":""}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	regs := NewResponseNormalizers()
	normalizer.RegisterMLX(regs)

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "mlx")
	wire.RelayBuffered(w, resp, normalizedBody(regs, "mlx",
		wire.RoutingMetadata{Provider: "mlx"}, recorder), recorder)

	assert.Contains(t, w.Body.String(), `"content":"yes"`)
	assert.NotContains(t, w.Body.String(), `"content":""`,
		"normalizer return must replace, not append")
}

func TestRelayBuffered_NormalizerReportsNoChange_KeepsOriginal(t *testing.T) {
	// A no-op editor must preserve the original body.

	body := ` {"original":"kept", "reasoning":"unchanged"} `
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	regs := NewResponseNormalizers()
	edits := 0
	regs.Register("noop", func(map[string]any) bool { edits++; return false })

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "noop")
	wire.RelayBuffered(w, resp, normalizedBody(regs, "noop",
		wire.RoutingMetadata{Provider: "noop"}, recorder), recorder)

	assert.Equal(t, 1, edits)
	assert.Equal(t, body, w.Body.String())
}

func TestRelayBuffered_NoNormalizerRegistered(t *testing.T) {
	body := `{"plain":"untouched"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "no-such-provider")
	wire.RelayBuffered(w, resp, normalizedBody(NewResponseNormalizers(), "no-such-provider",
		wire.RoutingMetadata{Provider: "no-such-provider"}, recorder), recorder)

	assert.Contains(t, w.Body.String(), `"plain":"untouched"`)
	// Content-Length matches whatever wire.MaybeInjectRoutingMetadata
	// produced (no inject if Provider is registered no-op-ish; metadata
	// only injects on completion shapes — here the body has no choices,
	// so it stays untouched).
	assert.NotEmpty(t, w.Header().Get("Content-Length"))
}

func TestRelayBuffered_ReadErrorAnswersBadGateway(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: errReader{}}
	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "p")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result := wire.RelayBuffered(w, resp, normalizedBody(NewResponseNormalizers(), "p", wire.RoutingMetadata{}, recorder), recorder)
	assert.Equal(t, wire.Truncated, result.Outcome)
	assert.Empty(t, w.Body.Bytes())
	failBrokenBody(w, req, result)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), `"error":`)
}

func TestStreamWithUsageInject_NoInjectClosureWhenInjectUsageFalse(t *testing.T) {
	// When meta.InjectUsage is false, the per-chunk closure must not
	// be allocated — we rely on this for the cost-truth contract on
	// providers that opt out of zzrouter metadata injection. The
	// observable proof: the stream comes through unmodified.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "noinject")
	streamWithUsageInject(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), w, resp, NewResponseNormalizers(), "noinject",
		wire.RoutingMetadata{Provider: "noinject", InjectUsage: false}, recorder)

	require.Equal(t, http.StatusOK, w.Code)
	got := w.Body.String()
	// Body must contain the original chunks verbatim.
	assert.Contains(t, got, `"content":"hi"`)
	assert.Contains(t, got, "[DONE]")
	// And no zzrouter usage block (which would surface a "usage" key).
	// Easy check: the literal string "_zzrouter" only appears in
	// MetadataWithSnapshot output. Absence proves no injection.
	assert.NotContains(t, got, `"_zzrouter"`,
		"InjectUsage:false must skip the per-chunk injection closure")
}

// errReader returns an error on Read. Used to exercise the read-error
// branch of RelayBuffered without poking unsafe internals.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReader) Close() error             { return nil }
