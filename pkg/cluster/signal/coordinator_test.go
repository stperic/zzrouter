package signal

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCoordinatorContinuesPollingDuringNestedPublication(t *testing.T) {
	s := workerExchange(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zzrouter/v1/internal/signals/poll":
			var poll Poll
			require.NoError(t, json.NewDecoder(r.Body).Decode(&poll))
			batch, err := s.Wait(r.Context(), poll)
			if err != nil {
				w.WriteHeader(503)
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(batch))
		case "/zzrouter/v1/internal/signals/ack":
			var ack Acknowledgment
			require.NoError(t, json.NewDecoder(r.Body).Decode(&ack))
			if !s.Acknowledge(ack) {
				w.WriteHeader(409)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	enabled := make(chan struct{})
	c := NewCoordinator(CoordinatorConfig{Client: srv.Client, Member: func(url, node string) bool { return url == srv.URL }, Handle: func(ctx context.Context, _ *x509.Certificate, op Operation) Result {
		switch op.Kind {
		case ProviderEnabled:
			// Mirrors provider sync requiring publication while the enable request is pending.
			err := s.NotifyCacheRefresh(ctx)
			if err != nil {
				return Result{Error: &StatusError{Status: 503, Detail: err.Error()}}
			}
			close(enabled)
		case Catalog:
		default:
			t.Errorf("unexpected operation %s", op.Kind)
		}
		return Result{}
	}})
	c.Start(t.Context())
	defer c.Stop(context.Background())
	c.Observe(srv.URL, srv.Certificate().Subject.CommonName)
	require.NoError(t, s.RecordProviderEnabled(t.Context(), "comfyui", true))
	<-enabled
}

type exchangeTransport func(*http.Request) (*http.Response, error)

func (f exchangeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func exchangeClient(t *testing.T, s *Signaler, ack func(*http.Request, Acknowledgment) error) *http.Client {
	t.Helper()
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "worker"}}
	return &http.Client{Transport: exchangeTransport(func(r *http.Request) (*http.Response, error) {
		var value any
		if strings.HasSuffix(r.URL.Path, "/poll") {
			var p Poll
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				return nil, err
			}
			batch, err := s.Wait(r.Context(), p)
			if err != nil {
				return nil, err
			}
			value = batch
		} else {
			var a Acknowledgment
			if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
				return nil, err
			}
			if err := ack(r, a); err != nil {
				return nil, err
			}
			value = map[string]bool{"acknowledged": true}
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}, nil
	})}
}

func TestCoordinatorReplaysCompletedAcknowledgment(t *testing.T) {
	for _, mode := range []string{"disconnect", "deadline", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := workerExchange(t)
				var acknowledgments, handled atomic.Int32
				client := exchangeClient(t, s, func(r *http.Request, ack Acknowledgment) error {
					if acknowledgments.Add(1) == 1 {
						if mode == "deadline" {
							<-r.Context().Done()
							return r.Context().Err()
						}
						if mode == "lost-response" {
							require.True(t, s.Acknowledge(ack))
						}
						return io.ErrUnexpectedEOF
					}
					require.True(t, s.Acknowledge(ack))
					return nil
				})
				c := NewCoordinator(CoordinatorConfig{Client: func() *http.Client { return client }, Member: func(string, string) bool { return true }, Handle: func(context.Context, *x509.Certificate, Operation) Result { handled.Add(1); return Result{} }})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				x := newExchange(ctx)
				defer x.stop()
				done := make(chan error, 1)
				go func() { done <- s.RecordProviderEnabled(ctx, "comfyui", true) }()
				require.Error(t, c.exchange(ctx, "https://worker", "worker", x))
				exchanged := make(chan error, 1)
				go func() { exchanged <- c.exchange(ctx, "https://worker", "worker", x) }()
				require.NoError(t, <-done)
				synctest.Wait()
				require.Equal(t, int32(2), acknowledgments.Load())
				require.Equal(t, int32(1), handled.Load())
				cancel()
				require.Error(t, <-exchanged)
			})
		})
	}
}

func TestCoordinatorIncarnationResetCancelsOldOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := workerExchange(t)
		client := exchangeClient(t, s, func(_ *http.Request, a Acknowledgment) error { require.True(t, s.Acknowledge(a)); return nil })
		entered, stopped := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		c := NewCoordinator(CoordinatorConfig{Client: func() *http.Client { return client }, Member: func(string, string) bool { return true }, Handle: func(ctx context.Context, _ *x509.Certificate, _ Operation) Result {
			if calls.Add(1) == 1 {
				close(entered)
				<-ctx.Done()
				close(stopped)
			}
			return Result{}
		}})
		c.Start(t.Context())
		defer c.Stop(context.Background())
		c.Observe("https://worker", "worker")
		old := make(chan error, 1)
		go func() { old <- s.RecordProviderEnabled(t.Context(), "comfyui", true) }()
		<-entered
		s.Reset()
		require.ErrorIs(t, <-old, context.Canceled)
		require.NoError(t, s.RecordProviderEnabled(t.Context(), "comfyui", false))
		<-stopped
		require.Equal(t, int32(2), calls.Load())
	})
}

func TestCoordinatorRestartDoesNotJoinNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := workerExchange(t)
		client := exchangeClient(t, s, func(_ *http.Request, a Acknowledgment) error { require.True(t, s.Acknowledge(a)); return nil })
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		c := NewCoordinator(CoordinatorConfig{Client: func() *http.Client { return client }, Member: func(string, string) bool { return true }, Handle: func(context.Context, *x509.Certificate, Operation) Result {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return Result{}
		}})
		c.Start(t.Context())
		c.Observe("https://worker", "worker")
		first := make(chan error, 1)
		go func() { first <- s.RecordProviderEnabled(t.Context(), "comfyui", true) }()
		<-entered
		c.mu.Lock()
		oldGeneration := c.generation
		c.mu.Unlock()
		deadline, cancel := context.WithTimeout(t.Context(), time.Second)
		c.Stop(deadline)
		cancel()
		s.Reset()
		require.ErrorIs(t, <-first, context.Canceled)
		c.Start(t.Context())
		c.Observe("https://worker", "worker")
		require.NoError(t, s.RecordProviderEnabled(t.Context(), "comfyui", false))
		close(release)
		oldGeneration.wg.Wait()
		c.Stop(context.Background())
	})
}
