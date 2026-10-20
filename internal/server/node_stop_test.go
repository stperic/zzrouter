package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestServerStopRunBudgetSurvivesExhaustedHTTPDrain(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	entered, closed := make(chan struct{}), make(chan struct{})
	drain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(closed)
	}))
	defer drain.Close()
	s.httpServer = drain.Config
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := drain.Client().Get(drain.URL)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	<-entered
	run := instance.NewInstance("drain-budget", "llamacpp", "model", 0, 0, 0)
	run.SetStatus(instance.StatusRunning)
	run.TrackGoroutine()
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	released := make(chan struct{})
	go func() { <-closed; time.Sleep(100 * time.Millisecond); run.GoroutineDone(); close(released) }()
	err := s.stop(20 * time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the real HTTP drain must exhaust its phase")
	<-released
	<-requestDone
	require.Equal(t, instance.StatusStopped, run.GetStatus(), "provider teardown must get a fresh budget after drain")
}

type shutdownContextProcessor struct {
	stop func(context.Context) error
}

func (shutdownContextProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (shutdownContextProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (shutdownContextProcessor) ForceFlush(context.Context) error                { return nil }
func (p shutdownContextProcessor) Shutdown(ctx context.Context) error            { return p.stop(ctx) }

func TestServerStopSlowEnginePreservesEpilogueBudget(t *testing.T) {
	for _, exhaustRunBudget := range []bool{false, true} {
		name := "slow engine"
		if exhaustRunBudget {
			name = "run deadline exhausted"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := createTestNodeWithDefaults(t)
				previous := otel.GetTracerProvider()
				t.Cleanup(func() { otel.SetTracerProvider(previous) })
				provider, err := observability.NewProvider(t.Context(), &observability.Config{Enabled: true, Tracing: observability.TracingConfig{Enabled: true}}, "test")
				require.NoError(t, err)
				s.otelProvider = provider
				flushed := false
				provider.TracerProvider().RegisterSpanProcessor(shutdownContextProcessor{stop: func(ctx context.Context) error {
					flushed = true
					require.NoError(t, ctx.Err(), "engine teardown must not expire the epilogue")
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					require.Equal(t, constants.ShutdownEpilogueTimeout, deadline.Sub(utils.Now()))
					time.Sleep(constants.ShutdownEpilogueTimeout - time.Second)
					return ctx.Err()
				}})
				run := instance.NewInstance("epilogue-budget", "llamacpp", "model", 0, 0, 0)
				run.SetStatus(instance.StatusRunning)
				run.TrackGoroutine()
				require.NoError(t, s.providers.appMgr.Instances().Register(run))
				delay := constants.ShutdownRunTimeout - 5*time.Second
				if exhaustRunBudget {
					delay = constants.ShutdownRunTimeout + time.Second
				}
				go func() { time.Sleep(delay); run.GoroutineDone() }()
				err = s.Stop()
				if exhaustRunBudget {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.NoError(t, err)
				}
				require.True(t, flushed, "the real OTel shutdown must run")
			})
		})
	}
}
