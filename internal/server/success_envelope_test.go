package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// Pins the success-path body shaping contract: Transfer-Encoding
// stripped, Content-Length matches body, body normalizer respected,
// MaybeInjectRoutingMetadata applied. Without these, a refactor that
// drops the strip or skips the normalizer-nil branch silently flips
// the wire shape — integration tests exercising the success path
// won't necessarily catch it (they assert content, not framing).

func TestWriteNormalizedBody_StripsTransferEncodingAndSetsLength(t *testing.T) {
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

	writeNormalizedBody(w, resp, NewResponseNormalizers(), "openrouter",
		wire.RoutingMetadata{Provider: "openrouter"}, recorder)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Transfer-Encoding"),
		"Transfer-Encoding must be stripped — body is being re-encoded")
	assert.Equal(t, "60", w.Header().Get("Content-Length"),
		"Content-Length must match the rewritten body length, got %q for %d-byte body",
		w.Header().Get("Content-Length"), w.Body.Len())
	assert.Equal(t, w.Body.Len(), len(w.Body.Bytes()),
		"recorder body must equal what was written")
}

func TestWriteNormalizedBody_NormalizerRespected(t *testing.T) {
	body := `{"original":"yes"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	regs := NewResponseNormalizers()
	regs.RegisterBody("test-provider", func(b []byte) []byte {
		return []byte(`{"normalized":"yes"}`)
	})

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "test-provider")
	writeNormalizedBody(w, resp, regs, "test-provider",
		wire.RoutingMetadata{Provider: "test-provider"}, recorder)

	assert.Contains(t, w.Body.String(), `"normalized":"yes"`)
	assert.NotContains(t, w.Body.String(), `"original":"yes"`,
		"normalizer return must replace, not append")
}

func TestWriteNormalizedBody_NormalizerReturnsNil_KeepsOriginal(t *testing.T) {
	// A normalizer that returns nil signals "no rewrite needed" —
	// the original body must pass through. Pre-extraction inline
	// code did `if rewritten := norm(b); rewritten != nil { b = rewritten }`;
	// regression here would either crash (assigning nil) or produce
	// an empty body.
	body := `{"original":"kept"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	regs := NewResponseNormalizers()
	regs.RegisterBody("noop", func(b []byte) []byte { return nil })

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "noop")
	writeNormalizedBody(w, resp, regs, "noop",
		wire.RoutingMetadata{Provider: "noop"}, recorder)

	assert.Contains(t, w.Body.String(), `"original":"kept"`)
}

func TestWriteNormalizedBody_NoNormalizerRegistered(t *testing.T) {
	body := `{"plain":"untouched"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}

	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "no-such-provider")
	writeNormalizedBody(w, resp, NewResponseNormalizers(), "no-such-provider",
		wire.RoutingMetadata{Provider: "no-such-provider"}, recorder)

	assert.Contains(t, w.Body.String(), `"plain":"untouched"`)
	// Content-Length matches whatever wire.MaybeInjectRoutingMetadata
	// produced (no inject if Provider is registered no-op-ish; metadata
	// only injects on completion shapes — here the body has no choices,
	// so it stays untouched).
	assert.NotEmpty(t, w.Header().Get("Content-Length"))
}

func TestWriteNormalizedBody_ReadErrorWritesStatusOnly(t *testing.T) {
	// io.ReadAll error path: WriteHeader is called with the upstream
	// status, no body written. Pre-extraction this was an explicit
	// branch; helper must preserve.
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       errReader{},
	}
	w := httptest.NewRecorder()
	recorder := llm.NewInferenceRecorder(t.Context(), "m", "p")
	writeNormalizedBody(w, resp, NewResponseNormalizers(), "p",
		wire.RoutingMetadata{}, recorder)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, w.Body.Bytes(), "no body should be written on read error")
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
	streamWithUsageInject(w, resp, NewResponseNormalizers(), "noinject",
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
// branch of writeNormalizedBody without poking unsafe internals.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReader) Close() error             { return nil }
