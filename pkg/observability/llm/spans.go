package llm

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// TracerName is the name of the tracer for LLM operations.
	TracerName = "zzrouter.llm"
)

// Tracer returns the LLM tracer.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// SpanKind constants for LLM operations.
const (
	// SpanKindInference is for inference requests (client to provider).
	SpanKindInference = trace.SpanKindClient

	// SpanKindModelLoad is for internal model loading operations.
	SpanKindModelLoad = trace.SpanKindInternal

	// SpanKindRouting is for internal routing decisions.
	SpanKindRouting = trace.SpanKindInternal
)

// StartInferenceSpan starts a span for an inference request.
func StartInferenceSpan(ctx context.Context, model, requestType string, streaming bool) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "llm.inference",
		trace.WithSpanKind(SpanKindInference),
		trace.WithAttributes(
			ModelName(model),
			RequestType(requestType),
			RequestStream(streaming),
		),
	)
}

// StartModelLoadSpan starts a span for a model load operation.
func StartModelLoadSpan(ctx context.Context, model, provider string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "llm.model.load",
		trace.WithSpanKind(SpanKindModelLoad),
		trace.WithAttributes(
			ModelName(model),
			ModelProvider(provider),
		),
	)
}

// SetSpanError sets the span status to error with the given error.
func SetSpanError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// SetSpanErrorWithType is SetSpanError plus an OTel `error.type` closed-enum
// attribute. Use this on pre-recorder error paths (request-bind failures,
// schema validation failures) where no InferenceRecorder ever runs to stamp
// the same attribute. errType comes from a closed vocabulary — see
// pkg/protocol/openai's ErrorCode for the canonical names.
func SetSpanErrorWithType(span trace.Span, errType string, err error) {
	if errType != "" {
		span.SetAttributes(ErrorType(errType))
	}
	SetSpanError(span, err)
}

// SetSpanOK sets the span status to OK.
func SetSpanOK(span trace.Span) {
	span.SetStatus(codes.Ok, "")
}

// AddModelLoadResult adds model load result attributes to the span.
func AddModelLoadResult(span trace.Span, loadTimeMs float64, instanceID string, port int) {
	span.SetAttributes(
		ModelLoadTime(loadTimeMs),
		InstanceID(instanceID),
		InstancePort(port),
	)
}
