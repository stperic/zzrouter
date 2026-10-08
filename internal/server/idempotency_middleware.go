// package server — Idempotency-Key middleware for POST /runs and
// /runs/ensure. Agents that retry on HTTP timeout (without changing
// the request body) attach a stable Idempotency-Key header; a duplicate
// POST with the same key + same body returns the original 2xx response
// instead of starting a second launch.
//
// Out of scope: cross-restart persistence (in-memory only — agents
// retry within seconds-to-minutes), worker-side dedupe (coord is the
// only entry point for /runs; coord-forwarded requests carry
// X-zzrouter-Cluster-Auth and bypass the gate).

package server

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/utils"
)

// idempotencyTTL is how long a recorded response is replayed. 24h
// matches Stripe's convention; agent retry windows during outages
// commonly span hours.
const idempotencyTTL = 24 * time.Hour

// idempotencyWaitTimeout caps the in-flight wait for a duplicate
// request whose original is still executing. The agent's HTTP timeout
// (typically 30s for /runs launches) would have fired before this; the
// bound exists to release a stuck goroutine if the original handler
// never publishes (e.g. panic that bypassed the deferred capture).
const idempotencyWaitTimeout = 30 * time.Second

// idempotencyHeader is the canonical request header. Match-case is
// http.Header — already canonicalized by Go.
const idempotencyHeader = "Idempotency-Key"

// idemKey scopes idempotency entries to (principal, path, header value).
// Principal isolation lets two virtual keys use the same Idempotency-Key
// string without collision; path isolation lets a single store back
// multiple routes (e.g. /model-groups/:name + /model-groups/:name/replicas/:replica)
// without one route's recorded response replaying for a different route.
type idemKey struct {
	principal string
	path      string
	value     string
}

// idemEntry is the stored result of the first request. inflight is
// closed by the writer wrapper once the handler has finished and the
// response has been captured. Subsequent duplicates block on inflight.
type idemEntry struct {
	bodyHash     [32]byte
	inflight     chan struct{}
	statusCode   int
	responseJSON []byte
	contentType  string
	expiresAt    time.Time
}

// idempotencyStore is the in-memory dedupe table. One mutex guards the
// map; entries themselves are read-only after their inflight channel
// closes, so no per-entry locking is needed.
type idempotencyStore struct {
	mu sync.Mutex
	m  map[idemKey]*idemEntry
}

// newIdempotencyStore returns an empty store.
func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{m: make(map[idemKey]*idemEntry)}
}

// idempotencyMiddleware returns a gin handler that dedupes POSTs
// carrying an Idempotency-Key header. Mount it on a per-route basis —
// it captures the response body, which is heavier than blanket
// middleware should be.
func (s *idempotencyStore) middleware() gin.HandlerFunc {
	return s.middlewareWithBodySkip(nil)
}

// middlewareWithBodySkip is like middleware but accepts a predicate that
// inspects the raw request body and short-circuits dedup when it returns
// true. Useful for routes whose response semantics make caching unsound
// for some bodies — e.g. /v1/chat/completions with `stream:true` would
// have its entire SSE event sequence buffered into the cache and replayed
// instantly, breaking live-stream timing for clients.
func (s *idempotencyStore) middlewareWithBodySkip(bodySkip func(body []byte) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Cluster-internal forwarded requests are already deduped at
		// the entry point; skip to keep storage at coord-only. Identified
		// by CtxKeyClusterTrusted set by the worker compat engine's
		// clusterTrustedMiddleware (cluster-port mTLS surface only).
		if trusted, _ := c.Get(string(CtxKeyClusterTrusted)); trusted == true {
			c.Next()
			return
		}
		headerVal := c.GetHeader(idempotencyHeader)
		if headerVal == "" {
			c.Next()
			return
		}

		// Read + restore body so the handler can re-bind it. Gin's
		// BindJSON drains c.Request.Body once; we replace it.
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Next()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		if bodySkip != nil && bodySkip(body) {
			c.Next()
			return
		}
		bodyHash := sha256.Sum256(body)

		key := idemKey{
			principal: PrincipalFromContext(c),
			// c.Request.URL.Path (resolved path) — not c.FullPath() (route
			// template) — so PATCH /model-groups/fast-chat and PATCH
			// /model-groups/code-gen scope separately under the same key.
			path:  c.Request.URL.Path,
			value: headerVal,
		}

		// Atomic check-and-create.
		s.mu.Lock()
		entry, found := s.m[key]
		if found && utils.Now().After(entry.expiresAt) {
			delete(s.m, key)
			found = false
		}
		if !found {
			entry = &idemEntry{
				bodyHash:  bodyHash,
				inflight:  make(chan struct{}),
				expiresAt: utils.Now().Add(idempotencyTTL),
			}
			s.m[key] = entry
			s.mu.Unlock()

			// First request: wrap writer, run handler, snapshot result.
			w := &capturingWriter{ResponseWriter: c.Writer}
			c.Writer = w
			c.Next()

			entry.statusCode = w.status()
			entry.responseJSON = w.body()
			entry.contentType = w.contentType()
			close(entry.inflight)
			return
		}
		s.mu.Unlock()

		// Duplicate request — same key, possibly different body.
		if entry.bodyHash != bodyHash {
			respondSurfaceError(c, httperr.Error{
				Status:  http.StatusUnprocessableEntity,
				Title:   "Idempotency Key Conflict",
				Type:    "invalid_request_error",
				Code:    string(openai.ErrorCodeIdempotencyKeyConflict),
				Message: "a different request body has already been recorded under this Idempotency-Key; either change the key or send the original body",
			})
			c.Abort()
			return
		}
		// Same body — wait for the original to finish and replay.
		select {
		case <-entry.inflight:
		case <-time.After(idempotencyWaitTimeout):
			respondSurfaceError(c, httperr.Error{
				Status:  http.StatusGatewayTimeout,
				Title:   "Idempotency Wait Timeout",
				Type:    "api_error",
				Code:    string(openai.ErrorCodeIdempotencyWaitTimeout),
				Message: "original idempotent request still in flight; retry shortly",
			})
			c.Abort()
			return
		case <-c.Request.Context().Done():
			c.AbortWithStatus(499) // client closed; gin/nginx convention
			return
		}
		if entry.contentType != "" {
			c.Header("Content-Type", entry.contentType)
		}
		c.Status(entry.statusCode)
		_, _ = c.Writer.Write(entry.responseJSON)
		c.Abort()
	}
}

// capturingWriter intercepts WriteHeader + Write so the middleware can
// replay the response on a later duplicate. It still forwards to the
// underlying ResponseWriter so the original caller sees the response
// in real time.
type capturingWriter struct {
	gin.ResponseWriter
	buf        bytes.Buffer
	statusCode int
	headerSet  bool
	mu         sync.Mutex
}

func (w *capturingWriter) WriteHeader(code int) {
	w.mu.Lock()
	w.statusCode = code
	w.headerSet = true
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w *capturingWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *capturingWriter) status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *capturingWriter) body() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func (w *capturingWriter) contentType() string {
	return w.Header().Get("Content-Type")
}
