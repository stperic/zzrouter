package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

// HTTPClient guards ordinary traffic while leaving the shared probe client raw.
func (sm *StateManager) HTTPClient(raw *http.Client) *http.Client {
	client := *raw
	base := client.Transport
	if base == nil {
		base = newConnectorTransport()
	}
	client.Transport = &nodeTransport{state: sm, base: base}
	return &client
}

type nodeTransport struct {
	state *StateManager
	base  http.RoundTripper
}

func (t *nodeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	// Fail closed: coordWorkerMTLSClient serves direct/fallback inference, provider sync, Ollama pulls and metrics.
	ep, err := t.state.endpoints.GetEndpointByURL(req.URL.Scheme + "://" + req.URL.Host + req.URL.EscapedPath())
	if err != nil || !ep.Usable() {
		return nil, fmt.Errorf("%w: node unavailable", fallback.ErrUnreachable)
	}
	if ep.IsLocal {
		return t.base.RoundTrip(req)
	}
	permit, err := ep.breaker.Acquire(false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", fallback.ErrUnreachable, err)
	}
	var tlsFailed atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) { tlsFailed.Store(err != nil) },
	}))
	finish := func(err error, complete bool) {
		outcome := transportOutcome(req.Context(), err, tlsFailed.Load())
		if err == nil && !complete {
			outcome = fallback.Neutral
		}
		result := permit.Complete(outcome)
		if !result.Applied || outcome == fallback.Neutral {
			return
		}
		r := t.state.endpoints
		r.mu.Lock()
		current := r.endpoints[ep.URL]
		if current == nil || current.breaker != ep.breaker || current.generation != ep.generation {
			r.mu.Unlock()
			return
		}
		if outcome == fallback.Failure {
			current.Snapshot.LastError = security.RedactSensitive(utils.SanitizeErrorMessage(err.Error()))
		} else {
			clearLastError(current)
		}
		before := current.Status
		r.mu.Unlock()
		if result.From == StateClosed && result.To == StateOpen && r.onTransition != nil {
			r.onTransition(ep.URL, Transition{From: before, To: StatusDown, At: utils.Now(), Err: err})
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		finish(err, false)
		return resp, err
	}
	resp.Body = &nodeBody{ReadCloser: resp.Body, finish: finish, success: resp.StatusCode < 400}
	return resp, nil
}

func (t *nodeTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func transportOutcome(ctx context.Context, err error, tlsFailed bool) fallback.Outcome {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return fallback.Neutral
	}
	if err == nil {
		return fallback.Success
	}
	var network net.Error
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if tlsFailed || errors.As(err, &network) || errors.As(err, &cert) || errors.As(err, &record) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fallback.Failure
	}
	return fallback.Neutral
}

type nodeBody struct {
	io.ReadCloser
	finish  func(error, bool)
	success bool
}

func (b *nodeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish(nil, b.success && errors.Is(err, io.EOF))
	}
	return n, err
}

func (b *nodeBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(nil, false)
	return err
}
