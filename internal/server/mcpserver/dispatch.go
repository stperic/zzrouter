package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// MaxResponseBytes bounds memory retained from a REST response.
	MaxResponseBytes = 1 << 20
	// errorExcerptBytes keeps unexpected HTTP errors short enough to act on.
	errorExcerptBytes = 512
)

// valueFree retains cancellation and deadlines without authentication values.
type valueFree struct{ context.Context }

func (valueFree) Value(any) any { return nil }

type dispatcher struct {
	cfg   Config
	outer *outerRequest
}

type routePrefix string

const (
	discoveryRoute routePrefix = "/zzrouter/v1"
	nodesRoute     routePrefix = "/zzrouter/v1/nodes"
	healthRoute    routePrefix = "/zzrouter/v1/health"
	jobsRoute      routePrefix = "/zzrouter/v1/jobs/"
)

type requestPath struct{ value string }

func (p routePrefix) path() requestPath { return requestPath{string(p)} }

// pathSegment admits one identifier, never an encoded path or query.
func pathSegment(prefix routePrefix, id string) (requestPath, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#%") {
		return requestPath{}, fmt.Errorf("invalid path identifier: use a nonempty id without /, \\, ?, #, %% or dot segments")
	}
	return requestPath{string(prefix) + url.PathEscape(id)}, nil
}

type request struct {
	method, contentType, idempotencyKey, ifMatch string
	path                                         requestPath
	query                                        url.Values
	body                                         []byte
}

func (d dispatcher) call(ctx context.Context, input request, out any) error {
	req, err := http.NewRequestWithContext(ctx, input.method, input.path.value, bytes.NewReader(input.body))
	if err != nil {
		return fmt.Errorf("build REST request: %w", err)
	}
	req.URL.RawQuery = input.query.Encode()
	if d.cfg.Streaming(input.method, req.URL.Path) != "" {
		return fmt.Errorf("streaming route %s is unavailable to tools; use a snapshot endpoint", req.URL.Path)
	}
	// A fresh cancellable context strips every outer value, including auth markers.
	inner, cancel := context.WithCancel(valueFree{ctx})
	defer cancel()
	stop := context.AfterFunc(d.outer.ctx, cancel) //nolint:contextcheck // Decision 2 bridges HTTP cancellation without inheriting auth values.
	defer stop()
	if d.outer.ctx.Err() != nil {
		cancel()
	}
	req = req.WithContext(context.WithValue(inner, dispatchKey, true)) //nolint:contextcheck // Decision 2 permits only the accounting marker in the value-free context.
	req.RemoteAddr = d.outer.remoteAddr
	for _, name := range append(append([]string(nil), d.cfg.KeyHeaders...), "X-Forwarded-For", "X-Real-IP") {
		for _, value := range d.outer.header.Values(name) {
			req.Header.Add(name, value)
		}
	}
	if input.contentType != "" {
		req.Header.Set("Content-Type", input.contentType)
	}
	if input.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", input.idempotencyKey)
	}
	if input.ifMatch != "" {
		req.Header.Set("If-Match", input.ifMatch)
	}
	req.Header.Set("X-Request-ID", fmt.Sprintf("%s.%d", d.outer.requestID, d.outer.index.Add(1)))
	if err := inner.Err(); err != nil {
		return fmt.Errorf("REST request interrupted: %w", err)
	}
	rec := &bufferedResponse{header: make(http.Header), cancel: cancel}
	d.cfg.Engine.ServeHTTP(rec, req)
	mediaType, _, _ := mime.ParseMediaType(rec.header.Get("Content-Type"))
	if mediaType == "application/problem+json" {
		var p utils.ProblemDetails
		if err := json.Unmarshal(rec.body.Bytes(), &p); err == nil {
			msg := fmt.Sprintf("HTTP %d %s: %s", rec.statusCode(), p.Code, p.Detail)
			for _, item := range p.Errors {
				msg += "\n" + item.Key + ": " + item.Message
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if rec.statusCode() < http.StatusOK || rec.statusCode() >= http.StatusMultipleChoices {
		excerpt := strings.TrimSpace(string(rec.body.Bytes()[:min(rec.body.Len(), errorExcerptBytes)]))
		return fmt.Errorf("HTTP %d: %s", rec.statusCode(), excerpt)
	}
	if rec.overflow {
		return fmt.Errorf("REST response exceeds the 1 MiB cap; narrow the query and retry")
	}
	if err := inner.Err(); err != nil {
		return fmt.Errorf("REST request interrupted: %w", err)
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return fmt.Errorf("decode REST response: %w", err)
	}
	return nil
}

type bufferedResponse struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
	cancel   context.CancelFunc
}

func (r *bufferedResponse) Header() http.Header { return r.header }
func (*bufferedResponse) Flush()                {}
func (r *bufferedResponse) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
func (r *bufferedResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *bufferedResponse) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if len(p) > MaxResponseBytes-r.body.Len() {
		_, _ = r.body.Write(p[:MaxResponseBytes-r.body.Len()])
		r.overflow = true
		r.cancel()
		return 0, fmt.Errorf("response cap exceeded")
	}
	return r.body.Write(p)
}
