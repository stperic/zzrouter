package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalRunCompletionContractFailureBeforeHandoff(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("daemon", config.ServiceConfig{
		Enabled: new(true), Mode: "external", Protocol: config.ProtocolOllama,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "http://daemon:11434"},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	transportCause := errors.New("synthetic connection refused")
	client := &http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) { return nil, transportCause })}
	var handed *instance.Instance
	observer := prov_apps.WithStartObserver(func(ctx context.Context, _ prov_apps.EnsureRequest) (context.Context, func(prov_apps.EnsureResult, error)) {
		return ctx, func(result prov_apps.EnsureResult, err error) {
			if err != nil || result.Run == nil {
				t.Errorf("no handed run: %v", err)
				return
			}
			handed = result.Run
			select {
			case <-handed.LeftStarting():
			case <-time.After(readinessWait):
				t.Error("load did not finish before handoff")
			}
			drain, cancel := context.WithTimeout(ctx, readinessWait)
			defer cancel()
			assert.NoError(t, handed.WaitForGoroutines(drain))
		}
	})
	mgr, err := prov_apps.NewProviderAppManager(cfg, prov_apps.WithHTTPClient(client), observer)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
	s := &Server{}
	s.providers.appMgr = mgr
	run, err := s.loadModelViaNodeAPI("m", "daemon", "chat")
	require.NotNil(t, handed)
	require.Equal(t, instance.StatusFailed, handed.GetStatus())
	assert.Nil(t, run)
	assert.ErrorIs(t, err, prov_apps.ErrProviderUnavailable)
	assert.ErrorIs(t, err, transportCause)
	assert.Equal(t, http.StatusServiceUnavailable, answeredStatus(err))
	assert.Empty(t, mgr.Instances().List())
}

func TestLocalRunCompletionContractOldCompletedRun(t *testing.T) {
	cause := errors.New("retained launch failure")
	for _, state := range []string{"ready", "failed", "replaced", "stopped", "pending"} {
		t.Run(state, func(t *testing.T) {
			run := instance.NewInstance("r", "daemon", "m", 0, 0, 1)
			run.StartedAt = utils.Now().Add(-constants.ModelStreamingTimeout - time.Minute)
			switch state {
			case "ready":
				run.MarkRunning(false, nil)
			case "failed", "replaced":
				run.MarkFailedError(cause)
				if state == "replaced" {
					run.SetStatus(instance.StatusStopped)
				}
			case "stopped":
				run.SetStatus(instance.StatusStopped)
			}
			s := &Server{}
			got, err := s.waitForLocalInstance(run)
			switch state {
			case "ready":
				require.NoError(t, err)
				assert.Same(t, run, got)
			case "failed", "replaced":
				assert.Nil(t, got)
				assert.ErrorIs(t, err, cause)
			case "stopped":
				assert.Nil(t, got)
				assert.ErrorIs(t, err, instance.ErrStopping)
			case "pending":
				assert.Nil(t, got)
				assert.ErrorIs(t, err, errLoadWarming)
				assert.Equal(t, "r", loadFailureWire(err).Extra["run_id"])
			}
		})
	}
}

func TestLocalRunCompletionContractWarmReuse(t *testing.T) {
	for _, state := range []instance.Status{instance.StatusRunning, instance.StatusUnhealthy, instance.StatusRestarting} {
		t.Run(string(state), func(t *testing.T) {
			mgr, err := prov_apps.NewProviderAppManager(aliasLoadConfig(t))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
			run := instance.NewInstance("warm", aliasLoadProvider, "local", 8300, time.Minute, 1)
			run.StartedAt = utils.Now().Add(-constants.ModelStreamingTimeout - time.Minute)
			run.ErrorMessage = "advisory diagnostic"
			run.SetStatus(state)
			require.NoError(t, mgr.Instances().Register(run))
			s := &Server{}
			s.providers.appMgr = mgr
			got, err := s.ensureLocalRun(prov_apps.EnsureRequest{Provider: aliasLoadProvider, Model: "local"})
			require.NoError(t, err)
			assert.Same(t, run, got)
			assert.Equal(t, state, got.GetStatus())
		})
	}
}
