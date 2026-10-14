package llm

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stperic/zzrouter/pkg/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Attempt owns one dispatch span through body consumption and resource release.
type Attempt struct {
	ctx  context.Context
	span trace.Span
}

// StartAttempt starts a client span only within an active request trace.
func StartAttempt(ctx context.Context, provider, model string) (context.Context, *Attempt) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, nil
	}
	p := ModelProvider(provider)
	p.Value = attribute.StringValue(observability.BoundMetadata(p.Value.AsString()))
	ctx, span := Tracer().Start(ctx, "llm.attempt", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		ModelName(observability.BoundMetadata(model)), p,
	))
	return ctx, &Attempt{ctx: ctx, span: span}
}

// SetTarget records the upstream host and port without credentials, path or query.
func (a *Attempt) SetTarget(target string) {
	if a == nil || !a.span.IsRecording() {
		return
	}
	u, err := url.Parse(target)
	if err != nil {
		return
	}
	a.span.SetAttributes(ServerAddress(observability.BoundMetadata(u.Hostname())))
	if port, err := strconv.Atoi(u.Port()); err == nil {
		a.span.SetAttributes(ServerPort(port))
	}
}

// SetStatus records the raw upstream status before response normalization.
func (a *Attempt) SetStatus(status int) {
	if a != nil {
		a.span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 400 {
			a.SetError("upstream_http_failure", nil)
		}
	}
}

// SetError records a closed category and never the raw error message.
func (a *Attempt) SetError(kind string, err error) {
	if a != nil {
		setSpanFailure(a.span, kind)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			SetSpanError(a.span, err)
		}
	}
}

// End is deferred by the dispatch owner after body close and lease release.
func (a *Attempt) End() {
	recovered := recover()
	if a != nil {
		SetSpanError(a.span, a.ctx.Err())
		if recovered == http.ErrAbortHandler {
			setSpanFailure(a.span, "client_cancellation")
		} else if recovered != nil {
			setSpanFailure(a.span, "internal_failure")
		}
		a.span.End()
	}
	if recovered != nil {
		panic(recovered)
	}
}
