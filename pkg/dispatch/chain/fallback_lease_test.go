package chain

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

func localGroupInstance(t *testing.T, deps *fakeDeps, queueTimeout time.Duration) *instance.Instance {
	t.Helper()
	inst := instance.NewInstance("run", "engine", "m", 8081, time.Minute, 1)
	inst.SetStatus(instance.StatusRunning)
	inst.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: queueTimeout})
	deps.instances = map[string]*instance.Instance{"m": inst}
	t.Cleanup(inst.Cancel)
	return inst
}

func leaseGroup(candidates []pfallback.Candidate) *resolver.Resolved {
	return &resolver.Resolved{OriginalName: "group", GroupName: "group", Strategy: "priority", Candidates: candidates}
}

func TestGroupRespectsInstanceCap(t *testing.T) {
	for _, wait := range []time.Duration{0, 10 * time.Millisecond} {
		t.Run(wait.String(), func(t *testing.T) {
			deps := newFakeDeps()
			inst := localGroupInstance(t, deps, wait)
			held, err := inst.Acquire(t.Context(), nil)
			require.NoError(t, err)
			defer held.Release()
			var localCalls atomic.Int32
			deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
				localCalls.Add(1)
				return cannedJSON(http.StatusOK, `{"model":"m","choices":[]}`), nil
			}
			deps.endpoints["next"] = "http://next.test"
			deps.forward["http://next.test/v1/chat/completions"] = func() (*http.Response, error) {
				assert.EqualValues(t, 1, inst.GetActiveRequests(), "the original lease is still held")
				return cannedJSON(http.StatusOK, `{"model":"m","choices":[]}`), nil
			}
			p := newProxyWithFake(deps)
			c, w := buildGinCtx(t, `{"model":"group"}`)
			resolved := leaseGroup([]pfallback.Candidate{
				{Name: "busy", App: "engine", Model: "m", OnDemand: true},
				{Name: "free", App: "next", Model: "m"},
			})

			provider := p.ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)

			assert.Equal(t, "next", provider)
			assert.Zero(t, localCalls.Load(), "a held slot must prevent sending to this instance")
			assert.EqualValues(t, 1, deps.forwardCalls.Load())
			assert.Equal(t, "2", w.Header().Get("X-zzrouter-Fallback-Count"), "capacity is recorded once before the next replica")
			assert.Contains(t, w.Body.String(), `"outcome":"retriable"`)
			assert.False(t, p.cooldowns.InCooldown("busy"))
			assert.False(t, p.providerCooldowns.InCooldown("engine"))
			assert.EqualValues(t, 1, inst.GetActiveRequests())
		})
	}
}

type observedLeaseBody struct {
	io.Reader
	inst    *instance.Instance
	reading chan int64
	proceed <-chan struct{}
	once    sync.Once
}

func (b *observedLeaseBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.reading <- b.inst.GetActiveRequests()
		<-b.proceed
	})
	return b.Reader.Read(p)
}

func (*observedLeaseBody) Close() error { return nil }

func TestIdleReaper_SparesGroupServedRun(t *testing.T) {
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, 0)
	reading, proceed := make(chan int64, 1), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(proceed) }) }
	defer unblock()
	deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
		resp := cannedJSON(http.StatusOK, `{"model":"m","choices":[]}`)
		resp.Body = &observedLeaseBody{Reader: strings.NewReader(`{"model":"m","choices":[]}`), inst: inst, reading: reading, proceed: proceed}
		return resp, nil
	}
	c, w := buildGinCtx(t, `{"model":"group"}`)
	resolved := leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		newProxyWithFake(deps).ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)
	}()
	select {
	case active := <-reading:
		assert.EqualValues(t, 1, active, "the idle reaper must see the group request while reading its body")
	case <-time.After(3 * time.Second):
		t.Error("group request did not begin reading")
	}
	assert.EqualValues(t, 1, inst.GetActiveRequests())
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("group request did not complete")
	}
	assert.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 0, inst.GetActiveRequests())
}

func TestFallbackRetryReleasesBeforeBackoff(t *testing.T) {
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, -1)
	inst.WireModel = "engine-token"
	firstSent := make(chan struct{})
	var calls atomic.Int32
	deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
		assert.EqualValues(t, 1, inst.GetActiveRequests(), "each attempt must hold its own lease")
		assert.Contains(t, string(deps.forwarded), `"model":"engine-token"`)
		if calls.Add(1) == 1 {
			close(firstSent)
			return cannedJSON(http.StatusServiceUnavailable, `{"error":{"message":"busy"}}`), nil
		}
		return cannedJSON(http.StatusOK, `{"model":"engine-token","choices":[]}`), nil
	}
	c, w := buildGinCtx(t, `{"model":"group"}`)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	resolved := leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		newProxyWithFake(deps).ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)
	}()
	select {
	case <-firstSent:
	case <-time.After(3 * time.Second):
		t.Fatal("first attempt did not reach the upstream")
	}
	if assert.Eventually(t, func() bool { return inst.GetActiveRequests() == 0 }, 300*time.Millisecond, 5*time.Millisecond, "the lease must be released during backoff") {
		lease, err := inst.Acquire(ctx, nil)
		assert.NoError(t, err, "another request can use the slot during backoff")
		if lease != nil {
			lease.Release()
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry deadlocked on its own lease")
	}
	assert.EqualValues(t, 2, calls.Load(), "the retry must acquire the cap-1 slot again")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "2", w.Header().Get("X-zzrouter-Fallback-Count"))
	assert.EqualValues(t, 0, inst.GetActiveRequests())
}

type leaseStreamWriter struct {
	gin.ResponseWriter
	inst    *instance.Instance
	writing chan int64
	proceed <-chan struct{}
}

func (w *leaseStreamWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[DONE]")) {
		w.writing <- w.inst.GetActiveRequests()
		<-w.proceed
	}
	return w.ResponseWriter.Write(p)
}

func TestStreamingReleasesAfterBody(t *testing.T) {
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, 0)
	reading, proceedRead := make(chan int64, 1), make(chan struct{})
	writing, proceedWrite := make(chan int64, 1), make(chan struct{})
	var readRelease, writeRelease sync.Once
	unblockRead := func() { readRelease.Do(func() { close(proceedRead) }) }
	unblockWrite := func() { writeRelease.Do(func() { close(proceedWrite) }) }
	defer unblockRead()
	defer unblockWrite()
	const frames = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: &observedLeaseBody{Reader: strings.NewReader(frames), inst: inst, reading: reading, proceed: proceedRead}}, nil
	}
	c, w := buildGinCtx(t, `{"model":"group","stream":true}`)
	c.Writer = &leaseStreamWriter{ResponseWriter: c.Writer, inst: inst, writing: writing, proceed: proceedWrite}
	resolved := leaseGroup([]pfallback.Candidate{{Name: "local", App: "engine", Model: "m", OnDemand: true}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		newProxyWithFake(deps).ProxyWithFallback(c, resolved, []byte(`{"model":"group","stream":true}`), nil, "group", false)
	}()
	select {
	case active := <-reading:
		assert.EqualValues(t, 1, active, "headers must not release the lease")
	case <-time.After(3 * time.Second):
		t.Error("stream did not begin reading")
	}
	unblockRead()
	select {
	case active := <-writing:
		assert.EqualValues(t, 1, active, "the lease must cover writing the final streamed frame")
	case <-time.After(3 * time.Second):
		t.Error("stream did not reach its final frame")
	}
	assert.EqualValues(t, 1, inst.GetActiveRequests())
	unblockWrite()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not complete")
	}
	assert.Contains(t, w.Body.String(), "data: [DONE]")
	assert.EqualValues(t, 0, inst.GetActiveRequests())
}

func TestGroupLeaseCancellationStopsChain(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			deps := newFakeDeps()
			inst := localGroupInstance(t, deps, -1)
			held, err := inst.Acquire(t.Context(), nil)
			require.NoError(t, err)
			defer held.Release()
			c, w := buildGinCtx(t, `{"model":"group"}`)
			ctx, cancel := context.WithCancel(c.Request.Context())
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(c.Request.Context(), utils.Now().Add(-time.Second))
			}
			cancel()
			c.Request = c.Request.WithContext(ctx)
			deps.endpoints["next"] = "http://next.test"
			resolved := leaseGroup([]pfallback.Candidate{
				{Name: "local", App: "engine", Model: "m", OnDemand: true},
				{Name: "next", App: "next", Model: "m"},
			})
			p := newProxyWithFake(deps)
			provider := p.ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)
			assert.Empty(t, provider)
			assert.Zero(t, deps.forwardCalls.Load())
			assert.Empty(t, w.Body.String())
			assert.False(t, c.Writer.Written())
			assert.False(t, p.cooldowns.InCooldown("local"))
			assert.EqualValues(t, 1, inst.GetActiveRequests())
		})
	}
}

func TestGroupClientGoneDuringBackoffStopsChain(t *testing.T) {
	deps := newFakeDeps()
	inst := localGroupInstance(t, deps, -1)
	c, w := buildGinCtx(t, `{"model":"group"}`)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	deps.forward["http://localhost:8081/v1/chat/completions"] = func() (*http.Response, error) {
		cancel()
		return cannedJSON(http.StatusServiceUnavailable, `{"error":{"message":"busy"}}`), nil
	}
	deps.endpoints["next"] = "http://next.test"
	resolved := leaseGroup([]pfallback.Candidate{
		{Name: "local", App: "engine", Model: "m", OnDemand: true},
		{Name: "next", App: "next", Model: "m"},
	})

	provider := newProxyWithFake(deps).ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)

	assert.Empty(t, provider)
	assert.EqualValues(t, 1, deps.forwardCalls.Load(), "no retry and no next replica for a client that left")
	assert.Empty(t, w.Body.String())
	assert.Zero(t, inst.GetActiveRequests())
}

type countingReader struct {
	r    io.Reader
	read *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// A retriable error body is read only up to the normalizer's bound, however large it is.
func TestErrorBodyDrainBounded(t *testing.T) {
	const huge = 8 << 20
	deps := newFakeDeps()
	var read atomic.Int64
	deps.endpoints["far"] = "http://far.test"
	deps.forward["http://far.test/v1/chat/completions"] = func() (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(countingReader{r: io.LimitReader(zeroReader{}, huge), read: &read})}, nil
	}
	c, _ := buildGinCtx(t, `{"model":"group"}`)
	resolved := leaseGroup([]pfallback.Candidate{{Name: "far", App: "far", Model: "m", MaxRetries: intPtr(1)}})

	newProxyWithFake(deps).ProxyWithFallback(c, resolved, []byte(`{"model":"group"}`), nil, "group", false)

	attempts := int64(deps.forwardCalls.Load())
	require.Positive(t, attempts)
	assert.LessOrEqual(t, read.Load(), attempts*(64<<10+1), "each attempt reads at most the bounded prefix")
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
