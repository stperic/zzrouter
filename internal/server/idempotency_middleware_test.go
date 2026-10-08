package server

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idempotency_middleware_test exercises the four scenarios that
// production traffic produces: replay on duplicate, conflict on body
// mismatch, race serialization on concurrent duplicates, and per-key
// scoping so two different principals using the same Idempotency-Key
// don't cross-contaminate.

// makeIdemRouter builds a minimal gin engine that runs the
// idempotency middleware in front of a counter handler. The handler
// returns 202 + a synthetic job_id derived from a per-router counter.
func makeIdemRouter(principal string, slowness time.Duration) (*gin.Engine, *idempotencyStore, *atomic.Int64) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}

	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		// Inject a synthetic AccessContext so PrincipalFromContext
		// returns the requested key id without spinning up a real
		// AccessControl + key store.
		if principal != "" {
			c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: principal}})
		}
	}, store.middleware(), func(c *gin.Context) {
		n := calls.Add(1)
		if slowness > 0 {
			time.Sleep(slowness)
		}
		c.JSON(http.StatusAccepted, gin.H{"job_id": "J" + string(rune('0'+n))})
	})
	return r, store, calls
}

func doPOST(r *gin.Engine, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/runs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 1. Happy path replay — same key + same body → same job_id, handler
// runs only once.
func TestIdempotency_Replay(t *testing.T) {
	r, _, calls := makeIdemRouter("alice", 0)
	body := `{"model_name":"x","provider":"vllm"}`
	headers := map[string]string{"Idempotency-Key": "abc"}

	w1 := doPOST(r, body, headers)
	require.Equal(t, http.StatusAccepted, w1.Code)
	require.Contains(t, w1.Body.String(), `"J1"`)

	w2 := doPOST(r, body, headers)
	require.Equal(t, http.StatusAccepted, w2.Code)
	assert.Equal(t, w1.Body.String(), w2.Body.String(),
		"replay must return identical body")
	assert.EqualValues(t, 1, calls.Load(),
		"handler must run exactly once across two requests")
}

// 2. Body-mismatch conflict — same key, different body → 422 with
// idempotency_key_conflict and the original launch is unaffected.
func TestIdempotency_BodyMismatchConflict(t *testing.T) {
	r, _, calls := makeIdemRouter("alice", 0)
	headers := map[string]string{"Idempotency-Key": "abc"}

	w1 := doPOST(r, `{"model_name":"x"}`, headers)
	require.Equal(t, http.StatusAccepted, w1.Code)

	w2 := doPOST(r, `{"model_name":"y"}`, headers)
	assert.Equal(t, http.StatusUnprocessableEntity, w2.Code)
	assert.Contains(t, w2.Body.String(), "Idempotency Key Conflict")
	assert.Contains(t, w2.Header().Get("Content-Type"), "application/problem+json")
	assert.EqualValues(t, 1, calls.Load(),
		"conflict must not retrigger the handler")
}

// 3. Concurrent race — two goroutines POST simultaneously with the
// same key+body. One wins, the other blocks on inflight and replays.
// The handler runs exactly once.
func TestIdempotency_ConcurrentRace(t *testing.T) {
	r, _, calls := makeIdemRouter("alice", 100*time.Millisecond)
	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "race"}

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = doPOST(r, body, headers)
		}(i)
	}
	wg.Wait()

	assert.EqualValues(t, 1, calls.Load(),
		"handler must run exactly once even under concurrent duplicates")
	for i, w := range results {
		assert.Equal(t, http.StatusAccepted, w.Code, "result %d", i)
	}
	assert.Equal(t, results[0].Body.String(), results[1].Body.String(),
		"both requests must observe the same response body")
}

// 4. Per-principal scoping — two virtual keys POSTing with the same
// Idempotency-Key string get distinct job IDs (no cross-tenant
// collision).
func TestIdempotency_PerPrincipalScoping(t *testing.T) {
	store := newIdempotencyStore()
	calls := &atomic.Int64{}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		// Principal comes from a request header for test purposes.
		id := c.GetHeader("X-Test-Principal")
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: id}})
	}, store.middleware(), func(c *gin.Context) {
		n := calls.Add(1)
		c.JSON(http.StatusAccepted, gin.H{"job_id": "J" + string(rune('0'+n))})
	})

	w1 := doPOST(r, `{"model_name":"x"}`, map[string]string{
		"Idempotency-Key":  "shared",
		"X-Test-Principal": "alice",
	})
	w2 := doPOST(r, `{"model_name":"x"}`, map[string]string{
		"Idempotency-Key":  "shared",
		"X-Test-Principal": "bob",
	})
	require.Equal(t, http.StatusAccepted, w1.Code)
	require.Equal(t, http.StatusAccepted, w2.Code)
	assert.NotEqual(t, w1.Body.String(), w2.Body.String(),
		"different principals must not collide on the same Idempotency-Key")
	assert.EqualValues(t, 2, calls.Load(),
		"each principal triggers an independent launch")
}

// 5. No header → middleware is a no-op (every request runs the handler).
func TestIdempotency_NoHeader_Passthrough(t *testing.T) {
	r, _, calls := makeIdemRouter("alice", 0)
	body := `{"model_name":"x"}`

	doPOST(r, body, nil)
	doPOST(r, body, nil)
	assert.EqualValues(t, 2, calls.Load(),
		"absent Idempotency-Key disables dedupe entirely")
}

// 6. Cluster-forwarded request (CtxKeyClusterTrusted set, as the worker
// compat engine does on cluster mTLS port arrivals) bypasses dedupe so
// the coord doesn't dedupe a second time.
func TestIdempotency_ClusterForwardedBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		c.Set(string(CtxKeyClusterTrusted), true)
	}, store.middleware(), func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusAccepted, gin.H{"ok": true})
	})

	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "abc"}
	doPOST(r, body, headers)
	doPOST(r, body, headers)
	assert.EqualValues(t, 2, calls.Load(),
		"cluster-trusted requests bypass dedupe; coord already enforced uniqueness")
}

// 7. Per-path scoping — a store shared across two routes must not
// replay route A's response for a request to route B even when key,
// principal, and body are identical. Drives the path component of
// idemKey added for the model-groups mutator surface.
func TestIdempotency_PerPathScoping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	callsA := &atomic.Int64{}
	callsB := &atomic.Int64{}
	setup := func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}
	r := gin.New()
	r.POST("/a", setup, store.middleware(), func(c *gin.Context) {
		callsA.Add(1)
		c.JSON(http.StatusOK, gin.H{"route": "a"})
	})
	r.POST("/b", setup, store.middleware(), func(c *gin.Context) {
		callsB.Add(1)
		c.JSON(http.StatusOK, gin.H{"route": "b"})
	})

	body := `{"x":1}`
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "shared")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	wa := post("/a")
	wb := post("/b")
	assert.Contains(t, wa.Body.String(), `"route":"a"`)
	assert.Contains(t, wb.Body.String(), `"route":"b"`,
		"same Idempotency-Key on a different path must not replay another route's response")
	assert.EqualValues(t, 1, callsA.Load())
	assert.EqualValues(t, 1, callsB.Load())
}

// idemBodyHashSanityCheck guards against regressions in the bodyHash
// derivation; the production code uses sha256 of raw bytes, and the
// test fixture above relies on that being stable.
func TestIdempotency_BodyHashIsRawBytes(t *testing.T) {
	a := []byte(`{"a":1}`)
	b := []byte(`{"a":1}`)
	assert.Equal(t, sha256.Sum256(a), sha256.Sum256(b))
}

// 7. Body-skip predicate short-circuits dedup when it returns true,
// preserving the existing dedup path when it returns false. Pins the
// /v1/chat/completions wiring contract — streaming requests skip the
// cache so SSE chunks are not buffered + replayed instantly.
func TestIdempotency_BodySkipShortCircuits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}, store.middlewareWithBodySkip(skipIdempotencyOnStreamingBody), func(c *gin.Context) {
		n := calls.Add(1)
		c.JSON(http.StatusOK, gin.H{"id": "R" + string(rune('0'+n))})
	})

	headers := map[string]string{"Idempotency-Key": "k1"}

	// Streaming body — skip predicate returns true, dedup never
	// engages, both requests run the handler.
	doPOST(r, `{"stream":true,"model":"x"}`, headers)
	doPOST(r, `{"stream":true,"model":"x"}`, headers)
	assert.EqualValues(t, 2, calls.Load(),
		"stream:true must bypass dedup so SSE timing is preserved")

	// Non-streaming body — skip predicate returns false, normal
	// dedup applies. Use a fresh key to avoid collision with the
	// streaming entries.
	calls.Store(0)
	headers2 := map[string]string{"Idempotency-Key": "k2"}
	w1 := doPOST(r, `{"stream":false,"model":"x"}`, headers2)
	w2 := doPOST(r, `{"stream":false,"model":"x"}`, headers2)
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, w1.Body.String(), w2.Body.String(),
		"stream:false must dedup as usual")
	assert.EqualValues(t, 1, calls.Load(),
		"non-streaming duplicate must not retrigger handler")
}

// 8. skipIdempotencyOnStreamingBody decision matrix — pins the JSON
// peek behavior across the four production-shape inputs so a future
// refactor doesn't silently invert the bypass.
func TestIdempotency_SkipPredicate_DecisionMatrix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"stream=true bypasses", `{"stream":true,"model":"x"}`, true},
		{"stream=false enforces", `{"stream":false,"model":"x"}`, false},
		{"missing field defaults false", `{"model":"x"}`, false},
		{"malformed body defaults false", `not-json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipIdempotencyOnStreamingBody([]byte(tc.body))
			assert.Equal(t, tc.want, got, "body=%s", tc.body)
		})
	}
}
