package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceAttemptPrivacyAndCancellation(t *testing.T) {
	spans := installSpanRecorder(t)
	_, off := StartAttempt(t.Context(), "openai", "m")
	require.Nil(t, off)
	off.End()
	ctx, parent := Tracer().Start(t.Context(), "logical")
	ctx, cancel := context.WithCancel(ctx)
	child, attempt := StartAttempt(ctx, "openai", strings.Repeat("m", 4096))
	attempt.SetTarget("https://user:private-secret@provider.test:443/path?key=private-secret")
	attempt.SetError("transport_failure", errors.New("private-secret"))
	cancel()
	attempt.End()
	parent.End()
	require.Len(t, spans.Ended(), 2)
	span := spans.Ended()[0]
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, trace.SpanContextFromContext(child), span.SpanContext())
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Empty(t, span.Events())
	for _, a := range span.Attributes() {
		assert.NotContains(t, a.Value.AsString(), "private-secret")
		assert.LessOrEqual(t, len(a.Value.AsString()), 512)
	}
	assert.NotContains(t, span.Status().Description, "private-secret")
}

func TestTraceSpanErrorPrivacy(t *testing.T) {
	spans := installSpanRecorder(t)
	_, span := Tracer().Start(t.Context(), "request")
	SetSpanErrorWithType(span, "invalid_request_error", errors.New("private-secret"))
	span.End()
	require.Len(t, spans.Ended(), 1)
	recorded := spans.Ended()[0]
	assert.Equal(t, codes.Error, recorded.Status().Code)
	assert.Empty(t, recorded.Events())
	assert.NotContains(t, recorded.Status().Description, "private-secret")
	assert.Equal(t, "invalid_request_error", attributesByKey(recorded.Attributes())["error.type"].AsString())
}

func TestTraceAttemptAbortHandlerPreservesCancellation(t *testing.T) {
	spans := installSpanRecorder(t)
	ctx, parent := Tracer().Start(t.Context(), "logical")
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		_, attempt := StartAttempt(ctx, "engine", "m")
		defer attempt.End()
		attempt.SetError("client_cancelled", nil)
		panic(http.ErrAbortHandler)
	})
	parent.End()
	require.Len(t, spans.Ended(), 2)
	assert.Equal(t, "client_cancellation", attributesByKey(spans.Ended()[0].Attributes())["error.type"].AsString())
}

func TestTraceAttemptKeepsClosedErrorCategory(t *testing.T) {
	spans := installSpanRecorder(t)
	ctx, parent := Tracer().Start(t.Context(), "logical")
	_, attempt := StartAttempt(ctx, "engine", "m")
	attempt.SetError("transport_failure", errors.New("private-secret"))
	attempt.End()
	parent.End()
	require.Len(t, spans.Ended(), 2)
	assert.Equal(t, "transport_failure", attributesByKey(spans.Ended()[0].Attributes())["error.type"].AsString())
}
