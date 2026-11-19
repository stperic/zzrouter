// Package server provides bounded replay for routes mounted through Server.idempotent.

package server

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/utils"
)

// idempotencyMaxEntries covers thousands of retries while bounding single-node metadata and captures.
const idempotencyMaxEntries = 4096

// idempotencyMaxBytes reserves a modest 32 MiB for replay bodies on a single node.
const idempotencyMaxBytes = 32 << 20

// idempotencyMaxEntryBytes keeps one large inference reply from dominating replay memory.
const idempotencyMaxEntryBytes = 1 << 20

// idempotencyStoreFullRetryAfter is short because in-flight entries complete within idempotencyWaitTimeout.
const idempotencyStoreFullRetryAfter = time.Second

// idempotencySweepInterval releases expired replies within minutes without frequent wakeups.
const idempotencySweepInterval = 5 * time.Minute

// idempotencyWaitTimeout caps the in-flight wait for a duplicate
// request whose original is still executing. The agent's HTTP timeout
// (typically 30s for /runs launches) would have fired before this; the
// bound exists to release a stuck goroutine if the original handler
// never publishes (e.g. panic that bypassed the deferred capture).
const idempotencyWaitTimeout = 30 * time.Second

// idempotencyHeader is the canonical request header. Match-case is
// http.Header — already canonicalized by Go.
const idempotencyHeader = "Idempotency-Key"

// idemKey scopes idempotency entries to (principal, method, path, canonical
// query, header value), so one store backs every route and verb without a
// recorded response replaying for a different request.
type idemKey struct {
	principal string
	method    string
	path      string
	query     string
	value     string
}

// idemEntry becomes immutable when inflight closes, so waiters need no entry lock.
type idemEntry struct {
	key          idemKey
	bodyHash     [32]byte
	inflight     chan struct{}
	statusCode   int
	responseJSON []byte
	contentType  string
	expiresAt    time.Time
	completed    *list.Element
}

// idempotencyStore is the bounded in-memory dedupe table one Server shares across routes.
type idempotencyStore struct {
	mu            sync.Mutex
	m             map[idemKey]*idemEntry
	completed     list.List // Completion order controls capacity eviction.
	bytes         int
	maxEntries    int
	maxBytes      int
	maxEntryBytes int
	now           func() time.Time
	routes        map[string]struct{} // "METHOD /path" of every route this store guards

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

// newIdempotencyStore returns an empty store with the production caps.
func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{
		m:             make(map[idemKey]*idemEntry),
		maxEntries:    idempotencyMaxEntries,
		maxBytes:      idempotencyMaxBytes,
		maxEntryBytes: idempotencyMaxEntryBytes,
		now:           utils.Now,
		routes:        make(map[string]struct{}),
	}
}

// Start ties the sweeper to the server lifetime and prevents duplicate goroutines.
func (s *idempotencyStore) Start(ctx context.Context) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.done != nil {
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(idempotencySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweep()
			}
		}
	}()
}

// Stop waits for the sweeper even when several shutdown callers race.
func (s *idempotencyStore) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
}

func (s *idempotencyStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for item := s.completed.Front(); item != nil; {
		entry := entryOf(item)
		item = item.Next()
		if !now.Before(entry.expiresAt) {
			s.forgetLocked(entry)
		}
	}
}

func (s *idempotencyStore) acquire(key idemKey, bodyHash [32]byte, arrivedAt time.Time) (*idemEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.m[key]; entry != nil {
		if entry.completed == nil || s.now().Before(entry.expiresAt) {
			return entry, false
		}
		s.forgetLocked(entry)
	}
	if !s.makeRoomLocked(1, 1) {
		return nil, false
	}
	entry := &idemEntry{key: key, bodyHash: bodyHash, inflight: make(chan struct{}), expiresAt: arrivedAt.Add(constants.IdempotencyReplayWindow)}
	s.m[key] = entry
	return entry, true
}

// makeRoomLocked only visits completed entries, so admission costs O(evicted).
func (s *idempotencyStore) makeRoomLocked(entries, bodyBytes int) bool {
	for len(s.m)+entries > s.maxEntries || s.bytes+bodyBytes > s.maxBytes {
		front := s.completed.Front()
		if front == nil {
			return false
		}
		s.forgetLocked(entryOf(front))
	}
	return true
}

func (s *idempotencyStore) complete(entry *idemEntry, status int, body []byte, contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.statusCode = status
	entry.contentType = contentType
	s.recordLocked(entry, body)
	retain := status >= 200 && status < 300
	if entry.key.method == http.MethodDelete && entry.key.path == "/zzrouter/v1/models" {
		var outcome utils.ProblemDetails
		if json.Unmarshal(body, &outcome) == nil && outcome.MutationApplied {
			retain = true
		}
	}
	if !retain || body == nil {
		if body == nil {
			entry.statusCode = 0 // An oversized response cannot be replayed; waiting duplicates must retry.
		}
		s.forgetLocked(entry)
	} else {
		entry.completed = s.completed.PushBack(entry)
	}
	close(entry.inflight)
	s.makeRoomLocked(0, 0)
}

// entryOf reads a completion-list element; the list holds only entries.
func entryOf(e *list.Element) *idemEntry {
	entry, _ := e.Value.(*idemEntry)
	return entry
}

func (s *idempotencyStore) recordLocked(entry *idemEntry, body []byte) {
	entry.responseJSON = body
	s.bytes += len(body)
}

func (s *idempotencyStore) forgetLocked(entry *idemEntry) {
	delete(s.m, entry.key)
	s.bytes -= len(entry.responseJSON)
	if entry.completed != nil {
		s.completed.Remove(entry.completed)
	}
}

// middlewareWithBodySkip bypasses bodies whose response cannot be replayed safely.
func (s *idempotencyStore) middlewareWithBodySkip(bodySkip func(body []byte) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		arrivedAt := s.now()
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
		principal := PrincipalFromContext(c)
		if principal == "" {
			// Anonymous callers share one principal, so recording would let one replay another's response.
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
			principal: principal,
			method:    c.Request.Method,
			// c.Request.URL.Path (resolved path) — not c.FullPath() (route
			// template) — so PATCH /model-groups/fast-chat and PATCH
			// /model-groups/code-gen scope separately under the same key.
			path: c.Request.URL.Path,
			// Encode sorts keys, so ?a=1&b=2 and ?b=2&a=1 are the same request.
			query: c.Request.URL.Query().Encode(),
			value: headerVal,
		}

		entry, first := s.acquire(key, bodyHash, arrivedAt)
		if entry == nil {
			respondSurfaceError(c, httperr.Error{
				Status:     http.StatusServiceUnavailable,
				Title:      "Idempotency Store Full",
				Type:       "api_error",
				Code:       string(openai.ErrorCodeIdempotencyStoreFull),
				Message:    "idempotency store is full with requests still in flight; retry shortly",
				RetryAfter: idempotencyStoreFullRetryAfter,
			})
			c.Abort()
			return
		}
		if first {
			w := &capturingWriter{ResponseWriter: c.Writer, maxBytes: s.maxEntryBytes}
			c.Writer = w
			var status int
			var response []byte
			var contentType string
			// A panic keeps status zero, but still forgets the entry and wakes its waiters.
			defer func() { s.complete(entry, status, response, contentType) }()
			c.Next()
			status = w.status()
			response = w.body()
			contentType = w.contentType()
			return
		}

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
		// Same body: wait for the original, but never replay beyond its arrival window.
		waitExpired := false
		select {
		case <-entry.inflight:
			waitExpired = !s.now().Before(entry.expiresAt)
		case <-time.After(idempotencyWaitTimeout):
			waitExpired = true
		case <-c.Request.Context().Done():
			c.AbortWithStatus(httperr.StatusClientClosedRequest)
			return
		}
		if waitExpired {
			respondSurfaceError(c, httperr.Error{
				Status:  http.StatusGatewayTimeout,
				Title:   "Idempotency Wait Timeout",
				Type:    "api_error",
				Code:    string(openai.ErrorCodeIdempotencyWaitTimeout),
				Message: "original idempotent request exceeded its wait or replay window; retry shortly",
			})
			c.Abort()
			return
		}
		if entry.statusCode == 0 {
			InternalNodeError(c, "the original request under this Idempotency-Key ended without a response; retry it")
			c.Abort()
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
	maxBytes   int
	oversized  bool
	mu         sync.Mutex
}

func (w *capturingWriter) WriteHeader(code int) {
	w.mu.Lock()
	w.statusCode = code
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.oversized {
		if len(p) > w.maxBytes-w.buf.Len() {
			w.oversized = true
			w.buf = bytes.Buffer{}
		} else {
			w.buf.Write(p)
		}
	}
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
	if w.oversized {
		return nil
	}
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func (w *capturingWriter) contentType() string {
	return w.Header().Get("Content-Type")
}
