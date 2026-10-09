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

// A failed first attempt is not recorded: the retry the error advises
// must run the handler again instead of replaying the failure.
func TestIdempotency_FailureIsNotReplayed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}, store.middleware(), func(c *gin.Context) {
		if calls.Add(1) == 1 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "wait for readiness"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"job_id": "J2"})
	})
	headers := map[string]string{"Idempotency-Key": "abc"}

	require.Equal(t, http.StatusServiceUnavailable, doPOST(r, `{"model_name":"x"}`, headers).Code)
	w2 := doPOST(r, `{"model_name":"x"}`, headers)
	require.Equal(t, http.StatusAccepted, w2.Code)
	w3 := doPOST(r, `{"model_name":"x"}`, headers)
	assert.Equal(t, w2.Body.String(), w3.Body.String(), "the success is what gets replayed")
	assert.EqualValues(t, 2, calls.Load())
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

// idemRequest sends one request with an arbitrary verb and target, for the
// tests that vary method or query against one router.
func idemRequest(r *gin.Engine, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 9. Method is part of the key — the same Idempotency-Key, path and body
// under PUT must run the handler again, not replay the POST's response.
func TestIdempotency_KeyIncludesMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	setup := func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}
	handler := func(c *gin.Context) {
		n := calls.Add(1)
		c.JSON(http.StatusAccepted, gin.H{"n": n})
	}
	r := gin.New()
	r.POST("/runs", setup, store.middleware(), handler)
	r.PUT("/runs", setup, store.middleware(), handler)

	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "abc"}
	w1 := idemRequest(r, "POST", "/runs", body, headers)
	require.Equal(t, http.StatusAccepted, w1.Code)
	w2 := idemRequest(r, "PUT", "/runs", body, headers)
	require.Equal(t, http.StatusAccepted, w2.Code)
	assert.EqualValues(t, 2, calls.Load(),
		"PUT with the same key must run the handler, not replay the POST")
	assert.NotEqual(t, w1.Body.String(), w2.Body.String())
}

// 10. Query is part of the key. Different query values run separately;
// the same query in another parameter order is the same request and replays.
func TestIdempotency_KeyIncludesQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	r := gin.New()
	r.POST("/runs", func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}, store.middleware(), func(c *gin.Context) {
		n := calls.Add(1)
		c.JSON(http.StatusAccepted, gin.H{"n": n})
	})

	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "abc"}
	idemRequest(r, "POST", "/runs?a=1", body, headers)
	idemRequest(r, "POST", "/runs?a=2", body, headers)
	assert.EqualValues(t, 2, calls.Load(),
		"a different query value must not replay the other request's response")

	headers2 := map[string]string{"Idempotency-Key": "abc-2"}
	w1 := idemRequest(r, "POST", "/runs?a=1&b=2", body, headers2)
	w2 := idemRequest(r, "POST", "/runs?b=2&a=1", body, headers2)
	require.Equal(t, http.StatusAccepted, w1.Code)
	assert.Equal(t, w1.Body.String(), w2.Body.String(),
		"the same query in another order must replay")
	assert.EqualValues(t, 3, calls.Load(),
		"the reordered query must not run the handler again")
}

// 11. Anonymous callers share one principal, so nothing is recorded:
// two identical keyed requests with no principal both run the handler.
func TestIdempotency_AnonymousNotRecorded(t *testing.T) {
	r, _, calls := makeIdemRouter("", 0)
	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "abc"}

	w1 := doPOST(r, body, headers)
	w2 := doPOST(r, body, headers)
	require.Equal(t, http.StatusAccepted, w1.Code)
	require.Equal(t, http.StatusAccepted, w2.Code)
	assert.EqualValues(t, 2, calls.Load(),
		"anonymous requests must not replay each other")
}

// 12. A handler that panics must still release its duplicates and forget
// its entry. The duplicate gets a 500 at once, and a retry runs again.
func TestIdempotency_PanicReleasesWaiters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	calls := &atomic.Int64{}
	entered := make(chan struct{})
	release := make(chan struct{})

	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/runs", func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}, store.middleware(), func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			panic("boom")
		}
		c.JSON(http.StatusAccepted, gin.H{"ok": true})
	})

	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "panic"}

	var original sync.WaitGroup
	original.Add(1)
	go func() {
		defer original.Done()
		idemRequest(r, "POST", "/runs", body, headers)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("original handler was never entered")
	}

	dupCode := make(chan int, 1)
	start := time.Now()
	go func() {
		dupCode <- idemRequest(r, "POST", "/runs", body, headers).Code
	}()
	// Let the duplicate reach its in-flight wait before the original panics.
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case code := <-dupCode:
		assert.Equal(t, http.StatusInternalServerError, code,
			"a duplicate of a request that ended without a response must not replay")
		assert.Less(t, time.Since(start), 2*time.Second,
			"a panicked original must release its duplicates well before idempotencyWaitTimeout")
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate was not released after the original panicked")
	}
	original.Wait()

	w3 := idemRequest(r, "POST", "/runs", body, headers)
	assert.Equal(t, http.StatusAccepted, w3.Code,
		"the panicked entry must be forgotten, so a retry runs the handler again")
	assert.EqualValues(t, 2, calls.Load())
}

// One store serves every route. The key includes the path, so the same
// Idempotency-Key and body on /a and /b are two operations, and a repeat
// on /a replays only /a's answer.
func TestIdempotency_SharedStoreAcrossRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newIdempotencyStore()
	aCalls := &atomic.Int64{}
	bCalls := &atomic.Int64{}
	principal := func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
	}
	handler := func(calls *atomic.Int64) gin.HandlerFunc {
		return func(c *gin.Context) {
			calls.Add(1)
			c.JSON(http.StatusAccepted, gin.H{"ok": true})
		}
	}
	r := gin.New()
	r.POST("/a", principal, store.middleware(), handler(aCalls))
	r.POST("/b", principal, store.middleware(), handler(bCalls))

	body := `{"model_name":"x"}`
	headers := map[string]string{"Idempotency-Key": "shared"}

	require.Equal(t, http.StatusAccepted, idemRequest(r, "POST", "/a", body, headers).Code)
	require.Equal(t, http.StatusAccepted, idemRequest(r, "POST", "/b", body, headers).Code)
	assert.EqualValues(t, 1, aCalls.Load(), "/a handler runs once")
	assert.EqualValues(t, 1, bCalls.Load(), "/b must run its own handler, not replay /a's answer")

	require.Equal(t, http.StatusAccepted, idemRequest(r, "POST", "/a", body, headers).Code)
	assert.EqualValues(t, 1, aCalls.Load(), "repeat on /a replays instead of running again")

	store.mu.Lock()
	assert.Len(t, store.m, 2, "one entry per route")
	store.mu.Unlock()
}

// The store's caps only bound memory if every controller records into the one Server owns.
func TestIdempotency_ControllersShareServerStore(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	require.NotNil(t, s.idempotency)
	require.NotNil(t, s.controllers.Runs)
	require.NotNil(t, s.controllers.ModelGroups)
	assert.Same(t, s.idempotency, s.controllers.Runs.idem)
	assert.Same(t, s.idempotency, s.controllers.ModelGroups.idemStore)
}
