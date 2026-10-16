package mesh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nodeTestPublic = "http://worker.invalid:9080"
const nodeTestCluster = "https://worker.invalid:9443"

type nodeTestTransport func(*http.Request) (*http.Response, error)

func (f nodeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nodeTestFailure() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func nodeTestResponse(r *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: make(http.Header), Body: body}
}

func nodeTestState(t *testing.T, rt http.RoundTripper) (*StateManager, *http.Client, *Dispatcher) {
	t.Helper()
	r := NewEndpointRegistry()
	require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: nodeTestCluster, NodeName: "worker", Alias: "alias"}))
	r.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{NodeName: "worker"}})
	sm := NewStateManager(r)
	raw := &http.Client{Transport: rt}
	d := NewDispatcher(&DispatcherConfig{Connector: NewConnector(ConnectorConfig{HTTPClient: raw}), State: sm, LocalHandler: &MockLocalHandler{}})
	return sm, sm.HTTPClient(raw), d
}

func nodeTestSend(t *testing.T, client *http.Client, address string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, address+"/request", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
	}
	return err
}

func nodeTestClock(t *testing.T) *clocktest.FakeClock {
	t.Helper()
	c := clocktest.NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	t.Cleanup(utils.SetClock(c))
	return c
}

type nodeTestBrokenBody struct{ err error }

func (b nodeTestBrokenBody) Read([]byte) (int, error) { return 0, b.err }
func (b nodeTestBrokenBody) Close() error             { return nil }

func TestNodeBreaker_TransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"connection", nodeTestFailure()},
		{"sanitized", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("Bearer zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab /Users/eric/private/credentials")}},
		{"dns", &net.DNSError{Err: "no such host", Name: "worker.invalid"}},
		{"tls", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
		{"tls record", tls.RecordHeaderError{}},
		{"tls verification callback", errors.New("peer OU mismatch")},
		{"reset before headers", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				sends++
				if tc.name == "tls verification callback" {
					httptrace.ContextClientTrace(r.Context()).TLSHandshakeDone(tls.ConnectionState{}, tc.err)
				}
				return nil, tc.err
			}))
			before, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			for i := 1; i <= 3; i++ {
				require.ErrorIs(t, nodeTestSend(t, client, nodeTestCluster), tc.err)
				assert.Equal(t, i, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic).Metrics()["failure_count"])
				want := StateClosed
				if i == 3 {
					want = StateOpen
				}
				assert.Equal(t, want, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic).State())
			}
			require.ErrorIs(t, nodeTestSend(t, client, nodeTestPublic), fallback.ErrUnreachable)
			assert.Equal(t, 3, sends)
			after, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			assert.Equal(t, StatusDown, after.Status)
			assert.Equal(t, before.Snapshot.CollectedAt, after.Snapshot.CollectedAt)
			assert.NotEmpty(t, after.Snapshot.LastError)
			assert.NotContains(t, after.Snapshot.LastError, "zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab")
			assert.NotContains(t, after.Snapshot.LastError, "/Users/eric/private/credentials")
		})
	}
}

func TestNodeBreaker_AllDispatchPaths(t *testing.T) {
	clock := nodeTestClock(t)
	for _, strategy := range []DispatchStrategy{StrategyUnicast, StrategyBroadcast, StrategyStreaming, StrategyRoundRobin, StrategyFailover} {
		t.Run(strategy.String(), func(t *testing.T) {
			sends := 0
			sm, _, d := nodeTestState(t, nodeTestTransport(func(*http.Request) (*http.Response, error) { sends++; return nil, nodeTestFailure() }))
			dispatch := func() {
				_, _ = d.Dispatch(context.Background(), strategy, sm.endpoints.GetAllEndpoints(), &Request{Method: "GET", Path: "/request"})
			}
			for range 3 {
				dispatch()
			}
			require.Equal(t, StateOpen, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic).State())
			dispatch()
			clock.Advance(time.Minute)
			dispatch()
			assert.Equal(t, 3, sends)
		})
	}
}

func TestNodeBreaker_NeutralOutcomes(t *testing.T) {
	for _, kind := range []string{"http400", "http429", "http500", "cancel", "cancel during body", "preexpired", "local transport", "local body", "close", "size cap"} {
		t.Run(kind, func(t *testing.T) {
			sends := 0
			sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				sends++
				if sends <= 2 {
					return nil, nodeTestFailure()
				}
				switch kind {
				case "cancel":
					return nil, context.Canceled
				case "local transport":
					return nil, errors.New("local request validation")
				case "local body":
					return nodeTestResponse(r, 200, nodeTestBrokenBody{errors.New("local decode")}), nil
				case "cancel during body":
					return nodeTestResponse(r, 200, nodeTestBrokenBody{context.Canceled}), nil
				}
				status := map[string]int{"http400": 400, "http429": 429, "http500": 500}[kind]
				if status == 0 {
					status = 200
				}
				return nodeTestResponse(r, status, io.NopCloser(strings.NewReader("reachable"))), nil
			}))
			for range 2 {
				require.Error(t, nodeTestSend(t, client, nodeTestCluster))
			}
			req, err := http.NewRequestWithContext(context.Background(), "GET", nodeTestCluster+"/request", nil)
			require.NoError(t, err)
			if kind == "preexpired" {
				ctx, cancel := context.WithDeadline(req.Context(), time.Unix(1, 0))
				defer cancel()
				req = req.WithContext(ctx)
			}
			resp, err := client.Do(req)
			if err == nil {
				switch kind {
				case "size cap":
					_, err = utils.ReadCapped(resp.Body, 2)
					require.Error(t, err)
				case "close":
				default:
					_, _ = io.Copy(io.Discard, resp.Body)
				}
				require.NoError(t, resp.Body.Close())
			}
			b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
			assert.Equal(t, StateClosed, b.State())
			assert.Equal(t, 2, b.Metrics()["failure_count"], "neutral evidence must not reset earlier failures")
		})
	}
}

func nodeTestMonitor(t *testing.T, mode *atomic.Int32, callback ReconnectionCallback) (*StateManager, *HealthMonitor) {
	t.Helper()
	rt := nodeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/zzrouter/v1/internal/version" {
			body, err := json.Marshal(version.VersionInfo{Version: &version.Version{Major: 0, Minor: 1}, APIVersion: "v1", ServiceType: version.ServiceTypeHost, ClusterProtocol: version.ClusterProtocolVersion, Capabilities: version.ClusterRequiredCapabilities})
			require.NoError(t, err)
			return nodeTestResponse(r, 200, io.NopCloser(strings.NewReader(string(body)))), nil
		}
		if mode.Load() == 2 {
			return nil, context.DeadlineExceeded
		}
		if mode.Load() == 1 && r.URL.Path != "/health" {
			return nil, nodeTestFailure()
		}
		return nodeTestResponse(r, 200, io.NopCloser(strings.NewReader(`{"hostname":"worker"}`))), nil
	})
	sm, _, _ := nodeTestState(t, rt)
	connector := NewConnector(ConnectorConfig{HTTPClient: &http.Client{Transport: rt}, ClusterPort: 9443})
	connector.maxRetries = 1
	hm := NewHealthMonitor(&HealthMonitorConfig{Connector: connector, Registry: sm.endpoints, ReconnectionCallback: callback})
	t.Cleanup(hm.Stop)
	return sm, hm
}

func nodeTestTrip(t *testing.T, b *CircuitBreaker) {
	t.Helper()
	for range 3 {
		p, err := b.Acquire(false)
		require.NoError(t, err)
		require.True(t, p.Complete(fallback.Failure).Applied)
	}
	require.Equal(t, StateOpen, b.State())
}

func TestNodeBreaker_ProbeRecovery(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, nil)
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	nodeTestTrip(t, b)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateOpen, b.State(), "full probe during cooldown must not recover")
	clock.Advance(time.Minute)
	mode.Store(1)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateOpen, b.State(), "public fallback must not recover")
	clock.Advance(time.Minute)
	mode.Store(0)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateHalfOpen, b.State())
	ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, ep.Status)
	assert.Equal(t, QualityFull, ep.Quality)
	assert.False(t, ep.Usable())
	_, err = b.Acquire(false)
	require.Error(t, err)
	mode.Store(2)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateOpen, b.State(), "failed probe must reset recovery streak")
	clock.Advance(time.Minute)
	mode.Store(0)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateHalfOpen, b.State())
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateClosed, b.State())
	ep, err = sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.True(t, ep.Usable())
}

func TestProbeDeadlineVersusShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			var mode atomic.Int32
			mode.Store(2)
			sm, hm := nodeTestMonitor(t, &mode, nil)
			if shutdown {
				hm.cancel()
			}
			ctx, cancel := context.WithDeadline(hm.ctx, time.Unix(1, 0))
			defer cancel()
			hm.probe(ctx, nodeTestPublic)
			ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			want := StatusDegraded
			if shutdown {
				want = StatusUp
			}
			assert.Equal(t, want, ep.Status)
		})
	}
}

func TestNodeUsabilityMatrix(t *testing.T) {
	clock := nodeTestClock(t)
	for _, kind := range []string{"local", "unknown", "up", "one full miss", "public only", "unknown miss", "down", "goodbye", "open", "halfopen"} {
		t.Run(kind, func(t *testing.T) {
			sends := 0
			sm, _, d := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				sends++
				return nodeTestResponse(r, 200, io.NopCloser(strings.NewReader("ok"))), nil
			}))
			r := sm.endpoints
			switch kind {
			case "local":
				r.mu.Lock()
				r.endpoints[nodeTestPublic].IsLocal = true
				r.endpoints[nodeTestPublic].Status = StatusDown
				r.mu.Unlock()
			case "unknown", "unknown miss":
				require.NoError(t, r.UnregisterEndpoint(nodeTestPublic))
				require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: nodeTestCluster, Status: StatusUnknown}))
				if kind == "unknown miss" {
					r.Observe(nodeTestPublic, ProbeResult{Err: nodeTestFailure()})
				}
			case "one full miss":
				r.Observe(nodeTestPublic, ProbeResult{Err: nodeTestFailure()})
			case "public only":
				r.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityDegraded})
			case "down":
				driveToDown(t, r, nodeTestPublic)
			case "goodbye":
				r.MarkDown(nodeTestPublic, errors.New("goodbye"))
			case "open", "halfopen":
				nodeTestTrip(t, r.breakers.GetBreaker(nodeTestPublic))
				if kind == "halfopen" {
					clock.Advance(time.Minute)
					p, err := r.breakers.GetBreaker(nodeTestPublic).Acquire(true)
					require.NoError(t, err)
					p.Complete(fallback.Success)
				}
			}
			usable := kind == "local" || kind == "up" || kind == "one full miss"
			ep, err := r.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			assert.Equal(t, usable, ep.Usable())
			for _, strategy := range []DispatchStrategy{StrategyUnicast, StrategyBroadcast, StrategyStreaming, StrategyRoundRobin, StrategyFailover} {
				sends = 0
				resp, _ := d.Dispatch(context.Background(), strategy, r.GetAllEndpoints(), &Request{Method: "GET", Path: "/request"})
				want := 0
				if usable && kind != "local" {
					want = 1
				}
				assert.Equal(t, want, sends, strategy.String())
				if strategy == StrategyBroadcast {
					require.NotNil(t, resp)
					assert.Len(t, resp.Nodes, want)
				}
			}
		})
	}
}

func TestNodeRecovery_ReconnectOnce(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	var callbacks atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, func(string) { callbacks.Add(1) })
	nodeTestTrip(t, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic))
	clock.Advance(time.Minute)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Zero(t, callbacks.Load())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { hm.probe(context.Background(), nodeTestPublic) })
	}
	wg.Wait()
	assert.EqualValues(t, 1, callbacks.Load())
	hm.probe(context.Background(), nodeTestPublic)
	assert.EqualValues(t, 1, callbacks.Load())
}

func TestNodeBreaker_ConcurrentLifecycle(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, nil)
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	late, err := b.Acquire(false)
	require.NoError(t, err)
	nodeTestTrip(t, b)
	clock.Advance(time.Minute)
	hm.probe(context.Background(), nodeTestPublic)
	assert.False(t, late.Complete(fallback.Success).Applied)
	assert.Equal(t, StateHalfOpen, b.State())
	clock.Advance(time.Second)
	for i := range 1100 {
		sm.GetCircuitBreakers().GetBreaker(fmt.Sprintf("transient-%d", i))
	}
	assert.Same(t, b, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic))
	reopen, err := b.Acquire(true)
	require.NoError(t, err)
	reopen.Complete(fallback.Failure)
	clock.Advance(2 * time.Hour)
	assert.Zero(t, sm.GetCircuitBreakers().PruneStale(time.Hour))
	require.NoError(t, sm.endpoints.RegisterEndpoint(&Endpoint{URL: nodeTestPublic}))
	assert.Same(t, b, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic))
	ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	probe, err := b.Acquire(true)
	require.NoError(t, err)
	require.NoError(t, sm.endpoints.UnregisterEndpoint(nodeTestPublic))
	require.NoError(t, sm.endpoints.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: nodeTestCluster, Status: StatusUnknown}))
	sm.endpoints.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{NodeName: "stale"}, endpoint: ep, permit: probe})
	current, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.Equal(t, StatusUnknown, current.Status)
	assert.Empty(t, current.NodeName)
	assert.NotSame(t, b, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic))
	client := sm.HTTPClient(hm.connector.httpClient)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			assert.NoError(t, sm.endpoints.UnregisterEndpoint(nodeTestPublic))
			assert.NoError(t, sm.endpoints.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: nodeTestCluster, Status: StatusUnknown}))
		}
	})
	for range 8 {
		wg.Go(func() {
			for range 20 {
				hm.probe(context.Background(), nodeTestPublic)
				_ = nodeTestSend(t, client, nodeTestCluster)
				_ = sm.endpoints.GetAllEndpoints()
			}
		})
	}
	wg.Wait()
}

func TestNodeBreaker_GoodbyeDuringRecovery(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, nil)
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	nodeTestTrip(t, b)
	clock.Advance(time.Minute)
	stale, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	permit, err := b.Acquire(true)
	require.NoError(t, err)
	sm.endpoints.MarkDown(nodeTestPublic, errors.New("goodbye"))
	sm.endpoints.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityFull, endpoint: stale, permit: permit})
	permit.Complete(fallback.Neutral)
	ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.Equal(t, StatusDown, ep.Status)
	assert.False(t, ep.Usable())
	assert.Equal(t, "goodbye", ep.Snapshot.LastError)
	hm.probe(context.Background(), nodeTestPublic)
	assert.Equal(t, StateHalfOpen, b.State(), "old full success must not advance recovery")
}

func TestNodeBreaker_AddressIdentity(t *testing.T) {
	for _, prefix := range []string{"", "/", "/prefix", "/prefix/"} {
		t.Run(prefix, func(t *testing.T) {
			r := NewEndpointRegistry()
			public, cluster := nodeTestPublic+prefix, nodeTestCluster+prefix
			require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: public, ClusterURL: cluster, NodeName: "worker"}))
			r.Observe(public, ProbeResult{OK: true, Quality: QualityFull})
			sends := 0
			client := NewStateManager(r).HTTPClient(&http.Client{Transport: nodeTestTransport(func(*http.Request) (*http.Response, error) { sends++; return nil, nodeTestFailure() })})
			for i, base := range []string{public, cluster, public} {
				require.Error(t, nodeTestSend(t, client, base))
				assert.Equal(t, i+1, r.breakers.GetBreaker(public).Metrics()["failure_count"])
			}
			require.ErrorIs(t, nodeTestSend(t, client, cluster), fallback.ErrUnreachable)
			assert.Equal(t, 3, sends)
		})
	}
}

func TestNodeBreaker_LocalTransportBypass(t *testing.T) {
	r := NewEndpointRegistry()
	require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, IsLocal: true, Status: StatusDown}))
	client := NewStateManager(r).HTTPClient(&http.Client{Transport: nodeTestTransport(func(*http.Request) (*http.Response, error) { return nil, nodeTestFailure() })})
	for range 4 {
		require.Error(t, nodeTestSend(t, client, nodeTestPublic))
	}
	assert.Equal(t, StateClosed, r.breakers.GetBreaker(nodeTestPublic).State())
	ep, err := r.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.True(t, ep.Usable())
}

func TestNodeBreaker_DefaultTransport(t *testing.T) {
	raw := &http.Client{Timeout: time.Second}
	client := NewStateManager(NewEndpointRegistry()).HTTPClient(raw)
	guard, ok := client.Transport.(*nodeTransport)
	require.True(t, ok)
	transport, ok := guard.base.(*http.Transport)
	require.True(t, ok)
	assert.Positive(t, transport.TLSHandshakeTimeout)
	assert.Positive(t, transport.ResponseHeaderTimeout)
	assert.NotNil(t, transport.DialContext)
	assert.Nil(t, raw.Transport)
	assert.Equal(t, raw.Timeout, client.Timeout)
	client.CloseIdleConnections()
}

func TestNodeBreaker_ReregisterMutableFields(t *testing.T) {
	sends := 0
	sm, client, _ := nodeTestState(t, nodeTestTransport(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, nodeTestFailure()
	}))
	for range 2 {
		require.Error(t, nodeTestSend(t, client, nodeTestCluster))
	}
	r := sm.endpoints
	before, err := r.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	pending, err := before.breaker.Acquire(false)
	require.NoError(t, err)
	const replacement = "https://worker.invalid:9555"
	require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: replacement, Alias: "new-alias", Name: "new-name", Status: StatusDown, IsLocal: true}))
	after, err := r.GetEndpointByURL(replacement + "/request")
	require.NoError(t, err)
	assert.Same(t, before.breaker, after.breaker)
	assert.Equal(t, before.generation, after.generation)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.Quality, after.Quality)
	assert.Equal(t, before.Snapshot, after.Snapshot)
	assert.False(t, after.IsLocal)
	assert.Equal(t, "worker", after.NodeName)
	require.True(t, pending.Complete(fallback.Neutral).Applied, "re-registration must retain the breaker generation")
	for _, selector := range []string{"new-alias", "new-name"} {
		assert.Len(t, r.GetEndpointsForRequest(&Request{TargetNode: selector}), 1)
	}
	assert.Empty(t, r.GetEndpointsForRequest(&Request{TargetNode: "alias"}))
	_, err = r.GetEndpointByURL(nodeTestCluster)
	require.Error(t, err, "the old cluster address must be deindexed")
	assert.NotContains(t, r.byName, "worker")
	require.Error(t, nodeTestSend(t, client, replacement))
	assert.Equal(t, 3, sends, "the new cluster address must reach the transport")
	assert.Equal(t, StateOpen, after.breaker.State(), "re-registration must preserve prior failure evidence")
}

func TestNodeBreaker_TimeoutOwnership(t *testing.T) {
	for _, kind := range []string{"request deadline", "client timeout", "transport header timeout"} {
		t.Run(kind, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			defer peer.Close()
			base := peer.Client().Transport.(*http.Transport).Clone()
			defer base.CloseIdleConnections()
			if kind == "transport header timeout" {
				base.ResponseHeaderTimeout = 20 * time.Millisecond
			}
			sends := 0
			r := NewEndpointRegistry()
			require.NoError(t, r.RegisterEndpoint(&Endpoint{URL: nodeTestPublic, ClusterURL: peer.URL}))
			r.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityFull})
			client := NewStateManager(r).HTTPClient(&http.Client{Transport: nodeTestTransport(func(req *http.Request) (*http.Response, error) {
				sends++
				if sends <= 2 {
					return nil, nodeTestFailure()
				}
				return base.RoundTrip(req)
			})})
			for range 2 {
				require.Error(t, nodeTestSend(t, client, peer.URL))
			}
			before, err := r.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			ctx := context.Background()
			if kind == "request deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			} else if kind == "client timeout" {
				client.Timeout = 20 * time.Millisecond
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.URL, nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			require.Error(t, err)
			var timeout net.Error
			require.ErrorAs(t, err, &timeout)
			require.True(t, timeout.Timeout())
			wantState, wantFailures := StateClosed, 2
			if kind == "transport header timeout" {
				wantState, wantFailures = StateOpen, 3
			}
			assert.Equal(t, wantState, before.breaker.State())
			assert.Equal(t, wantFailures, before.breaker.Metrics()["failure_count"])
			after, err := r.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			if kind != "transport header timeout" {
				assert.Equal(t, before.Snapshot.LastError, after.Snapshot.LastError)
			}
			assert.Equal(t, before.Snapshot.CollectedAt, after.Snapshot.CollectedAt)
		})
	}
}

func TestNodeBreaker_DoneContextNeutral(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
			}
			sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				if !deadline {
					cancel()
				}
				<-r.Context().Done()
				return nil, nodeTestFailure()
			}))
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeTestCluster, nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			require.Error(t, err)
			b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
			assert.Zero(t, b.Metrics()["failure_count"])
		})
	}
}

func TestNodeBreaker_BodyErrorsNeutral(t *testing.T) {
	for _, bodyErr := range []error{io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, context.DeadlineExceeded} {
		t.Run(bodyErr.Error(), func(t *testing.T) {
			sends := 0
			sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				sends++
				if sends <= 2 {
					return nil, nodeTestFailure()
				}
				return nodeTestResponse(r, http.StatusOK, nodeTestBrokenBody{bodyErr}), nil
			}))
			for range 2 {
				require.Error(t, nodeTestSend(t, client, nodeTestCluster))
			}
			before, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			for range 4 {
				require.ErrorIs(t, nodeTestSend(t, client, nodeTestCluster), bodyErr)
			}
			after, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
			require.NoError(t, err)
			assert.Equal(t, StateClosed, after.breaker.State())
			assert.Equal(t, 2, after.breaker.Metrics()["failure_count"])
			assert.Equal(t, before.Snapshot, after.Snapshot)
			assert.Equal(t, StatusUp, after.Status)
		})
	}
}

func TestNodeBreaker_TrafficTransitionOnce(t *testing.T) {
	clock := nodeTestClock(t)
	var mode, reconnects atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, func(string) { reconnects.Add(1) })
	var mu sync.Mutex
	var transitions []Transition
	handler := sm.endpoints.onTransition
	sm.endpoints.onTransition = func(url string, tr Transition) {
		if !tr.IsZero() {
			// Reading the registry here also proves hooks run outside its lock.
			_, err := sm.endpoints.GetEndpointByURL(url)
			assert.NoError(t, err)
			mu.Lock()
			transitions = append(transitions, tr)
			mu.Unlock()
		}
		handler(url, tr)
	}
	client := sm.HTTPClient(&http.Client{Transport: nodeTestTransport(func(*http.Request) (*http.Response, error) { return nil, nodeTestFailure() })})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _ = nodeTestSend(t, client, nodeTestCluster) })
	}
	wg.Wait()
	require.Len(t, transitions, 1)
	assert.Equal(t, StatusUp, transitions[0].From)
	assert.Equal(t, StatusDown, transitions[0].To)
	assert.NotZero(t, transitions[0].At)
	assert.Error(t, transitions[0].Err)
	assert.Zero(t, reconnects.Load())
	clock.Advance(time.Minute)
	hm.checkAllNodes()
	hm.checkAllNodes()
	hm.checkAllNodes()
	require.Len(t, transitions, 2)
	assert.Equal(t, StatusDegraded, transitions[1].From)
	assert.Equal(t, StatusUp, transitions[1].To)
	assert.EqualValues(t, 1, reconnects.Load())
}

func TestNodeBreaker_CompletionLockScope(t *testing.T) {
	for _, kind := range []string{"accepted", "stale", "completed", "neutral"} {
		t.Run(kind, func(t *testing.T) {
			sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
				return nodeTestResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(""))), nil
			}))
			r := sm.endpoints
			b := r.breakers.GetBreaker(nodeTestPublic)
			p, err := b.Acquire(false)
			require.NoError(t, err)
			p.Complete(fallback.Failure)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, nodeTestCluster, nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			switch kind {
			case "stale":
				for range 2 {
					p, err := b.Acquire(false)
					require.NoError(t, err)
					p.Complete(fallback.Failure)
				}
			case "completed":
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
			}
			// A registry reader must not block breaker completion. Stale/duplicate
			// completions must finish entirely without a registry write lock.
			r.mu.RLock()
			done := make(chan struct{})
			go func(body io.ReadCloser) {
				if kind != "neutral" {
					_, _ = io.Copy(io.Discard, body)
				}
				_ = body.Close()
				close(done)
			}(resp.Body)
			if kind == "accepted" {
				assert.Eventually(t, func() bool { return b.Metrics()["failure_count"] == 0 }, time.Second, time.Millisecond)
			} else {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("completion waited for the registry write lock")
				}
			}
			r.mu.RUnlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("completion did not drain")
			}
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestNodeBreaker_LastErrorClearingRule(t *testing.T) {
	for _, path := range []string{"traffic", "probe"} {
		for _, full := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/full=%v", path, full), func(t *testing.T) {
				sends := 0
				sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
					sends++
					if sends <= 2 {
						return nil, nodeTestFailure()
					}
					return nodeTestResponse(r, http.StatusOK, io.NopCloser(strings.NewReader("ok"))), nil
				}))
				for range 2 {
					require.Error(t, nodeTestSend(t, client, nodeTestCluster))
				}
				before, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
				require.NoError(t, err)
				require.NotEmpty(t, before.Snapshot.LastError)
				if path == "traffic" {
					req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, nodeTestCluster, nil)
					require.NoError(t, err)
					resp, err := client.Do(req)
					require.NoError(t, err)
					if !full {
						sm.endpoints.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: QualityDegraded})
					}
					_, err = io.Copy(io.Discard, resp.Body)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				} else {
					quality := QualityDegraded
					if full {
						quality = QualityFull
					}
					sm.endpoints.Observe(nodeTestPublic, ProbeResult{OK: true, Quality: quality})
				}
				after, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
				require.NoError(t, err)
				if full {
					assert.Empty(t, after.Snapshot.LastError)
				} else {
					assert.Equal(t, before.Snapshot.LastError, after.Snapshot.LastError)
				}
			})
		}
	}
}

func TestNodeBreaker_UnregisteredURLRefused(t *testing.T) {
	sends := 0
	sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
		sends++
		return nodeTestResponse(r, http.StatusOK, io.NopCloser(strings.NewReader("ok"))), nil
	}))
	for _, address := range []string{"https://unknown.invalid:9443", "https://worker.invalid:9444", "http://worker.invalid:9443"} {
		require.ErrorIs(t, nodeTestSend(t, client, address), fallback.ErrUnreachable)
	}
	assert.Zero(t, sends)
	assert.Zero(t, sm.GetCircuitBreakers().GetBreaker(nodeTestPublic).Metrics()["failure_count"])
	require.NoError(t, nodeTestSend(t, client, nodeTestCluster))
	assert.Equal(t, 1, sends)
}

func TestCheckAllNodes_RecoversOpenBreaker(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, nil)
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	nodeTestTrip(t, b)
	clock.Advance(time.Minute)
	hm.checkAllNodes()
	assert.Equal(t, StateHalfOpen, b.State())
	hm.checkAllNodes()
	assert.Equal(t, StateClosed, b.State())
}

func TestNodeBreaker_GoodbyeInvalidatesTraffic(t *testing.T) {
	sm, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
		return nodeTestResponse(r, http.StatusOK, io.NopCloser(strings.NewReader("ok"))), nil
	}))
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	p, err := b.Acquire(false)
	require.NoError(t, err)
	p.Complete(fallback.Failure)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, nodeTestCluster, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	sm.endpoints.MarkDown(nodeTestPublic, errors.New("goodbye"))
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, b.Metrics()["failure_count"], "late traffic must not reset evidence after goodbye")
	ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	assert.Equal(t, StatusDown, ep.Status)
	assert.Equal(t, "goodbye", ep.Snapshot.LastError)
}

func TestNodeBreaker_LastErrorRecovery(t *testing.T) {
	clock := nodeTestClock(t)
	var mode atomic.Int32
	sm, hm := nodeTestMonitor(t, &mode, nil)
	client := sm.HTTPClient(&http.Client{Transport: nodeTestTransport(func(*http.Request) (*http.Response, error) { return nil, nodeTestFailure() })})
	for range 3 {
		require.Error(t, nodeTestSend(t, client, nodeTestCluster))
	}
	ep, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
	require.NoError(t, err)
	require.NotEmpty(t, ep.Snapshot.LastError)
	check := func(state CircuitBreakerState, cleared bool) {
		t.Helper()
		hm.checkAllNodes()
		current, err := sm.endpoints.GetEndpointByURL(nodeTestPublic)
		require.NoError(t, err)
		assert.Equal(t, state, current.breaker.State())
		if cleared {
			assert.Empty(t, current.Snapshot.LastError)
		} else {
			assert.Equal(t, ep.Snapshot.LastError, current.Snapshot.LastError)
		}
	}
	check(StateOpen, false)
	clock.Advance(time.Minute)
	check(StateHalfOpen, false)
	check(StateClosed, true)
}

func TestNodeBreaker_TrafficTransitionCurrentStatus(t *testing.T) {
	var sm *StateManager
	sm, client, _ := nodeTestState(t, nodeTestTransport(func(*http.Request) (*http.Response, error) {
		sm.endpoints.Observe(nodeTestPublic, ProbeResult{Err: nodeTestFailure()})
		return nil, nodeTestFailure()
	}))
	b := sm.GetCircuitBreakers().GetBreaker(nodeTestPublic)
	for range 2 {
		p, err := b.Acquire(false)
		require.NoError(t, err)
		p.Complete(fallback.Failure)
	}
	var transitions []Transition
	sm.endpoints.onTransition = func(_ string, tr Transition) { transitions = append(transitions, tr) }
	require.Error(t, nodeTestSend(t, client, nodeTestCluster))
	require.Len(t, transitions, 1)
	assert.Equal(t, StatusDegraded, transitions[0].From, "report current probe evidence, not admission-time status")
	assert.Equal(t, StatusDown, transitions[0].To)
}
