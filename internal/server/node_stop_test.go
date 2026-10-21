package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
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

func TestServerStopRunDrainPreservesEpilogueBudget(t *testing.T) {
	for _, tc := range []struct {
		name, grace string
		delay       time.Duration
		exhausted   bool
	}{
		{"slow engine", "gpu", constants.ShutdownRunTimeout - time.Second, false},
		{"stop budget exhausted", "1s", time.Second + constants.ShutdownRunTailTimeout + time.Second, true},
		{"node run deadline exhausted", "2m", constants.ShutdownRunTimeout + constants.ShutdownRunTailTimeout + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := createTestNodeWithDefaults(t)
				cfg := &config.AppsConfig{}
				require.NoError(t, cfg.AddApp("llamacpp", config.ServiceConfig{
					Name: "llamacpp", Enabled: new(true), Mode: "on-demand", Protocol: config.ProtocolOpenAI,
					Runtime:      &config.AppRuntimeConfig{BasePort: 8600, Execution: config.ExecutionConfig{Type: "cli", Command: "test-only"}},
					Defaults:     &config.AppDefaultsConfig{Metadata: &config.AppMetadata{ShutdownTimeout: tc.grace}},
					Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
				}))
				s.providers.appMgr.ReloadConfig(cfg)
				previous := otel.GetTracerProvider()
				t.Cleanup(func() { otel.SetTracerProvider(previous) })
				provider, err := observability.NewProvider(t.Context(), &observability.Config{Enabled: true, Tracing: observability.TracingConfig{Enabled: true}}, "test")
				require.NoError(t, err)
				s.otelProvider = provider
				flushed := false
				var started time.Time
				provider.TracerProvider().RegisterSpanProcessor(shutdownContextProcessor{stop: func(ctx context.Context) error {
					flushed = true
					require.NoError(t, ctx.Err(), "engine teardown must not expire the epilogue")
					assert.Less(t, constants.ShutdownDrainTimeout+utils.Now().Sub(started)+constants.ShutdownEpilogueTimeout, constants.SupervisorStopTimeout, "actual run teardown must leave the other phase reservations and a supervisor margin")
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					require.Equal(t, constants.ShutdownSubsystemTimeout, deadline.Sub(utils.Now()))
					time.Sleep(constants.ShutdownSubsystemTimeout - time.Second)
					return ctx.Err()
				}})
				run := instance.NewInstance("epilogue-budget", "llamacpp", "model", 0, 0, 0)
				run.SetStatus(instance.StatusRunning)
				run.TrackGoroutine()
				require.NoError(t, s.providers.appMgr.Instances().Register(run))
				go func() { time.Sleep(tc.delay); run.GoroutineDone() }()
				started = utils.Now()
				err = s.Stop()
				if tc.exhausted {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.True(t, s.providers.appMgr.Instances().Has(run.ID))
					require.True(t, run.StopUnconfirmed.Load())
				} else {
					require.NoError(t, err)
					require.False(t, s.providers.appMgr.Instances().Has(run.ID))
				}
				require.True(t, flushed, "the real OTel shutdown must run")
			})
		})
	}
}
