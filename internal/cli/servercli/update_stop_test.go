package servercli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

type stopSettingsService struct {
	service.ServiceManager
	effects *[]string
	fail    error
}

func (s stopSettingsService) IsFirstRun() bool { return false }
func (s stopSettingsService) SetupDirectories() error {
	*s.effects = append(*s.effects, "directories")
	return nil
}
func (s stopSettingsService) InstallService() error {
	*s.effects = append(*s.effects, "install and stop settings")
	return nil
}
func (s stopSettingsService) EnsureStopSettings() error {
	*s.effects = append(*s.effects, "stop settings")
	return s.fail
}
func (s stopSettingsService) RestartService() error {
	*s.effects = append(*s.effects, "restart")
	return nil
}

func TestUpdateEnablePreservesLayoutBeforeInstallation(t *testing.T) {
	var effects []string
	svc := stopSettingsService{effects: &effects}
	require.NoError(t, enableManagedUpdatesWith(false, svc, func() (string, bool, error) {
		effects = append(effects, "layout")
		return "/opt/zzrouter/bin/zzrouter-node", false, nil
	}))
	require.Equal(t, []string{"directories", "layout", "install and stop settings"}, effects)
}

type stopSettingsScheduler struct {
	installerScheduler
	effects *[]string
	restart *deferredRestart
}

func (s stopSettingsScheduler) ApplyNow(context.Context) error {
	*s.effects = append(*s.effects, "apply")
	return s.restart.Restart()
}

func TestPrivilegedRestartAppliesStopSettingsBeforeRestart(t *testing.T) {
	for _, action := range []update.RequestAction{update.ActionApply, update.ActionRollback} {
		for _, fail := range []bool{false, true} {
			name := string(action) + "/success"
			if fail {
				name = string(action) + "/reload fails"
			}
			t.Run(name, func(t *testing.T) {
				var effects []string
				svc := stopSettingsService{effects: &effects}
				if fail {
					svc.fail = errors.New("reload failed")
				}
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				_, h, installer := updateCLIFixture(t)
				scheduler := update.NewSchedulerIn(t.TempDir(), &config.UpdateConfig{}, clock.System())
				defer scheduler.Stop()
				restart := &deferredRestart{}
				r := &privilegedRun{handoff: h, manager: svc,
					sched:   stopSettingsScheduler{installerScheduler{Scheduler: scheduler, installer: installer}, &effects, restart},
					restart: restart, req: &update.Request{Action: action},
					probe: updateTestProbe{check: func() error {
						status, err := h.LastRun()
						require.NoError(t, err)
						if fail {
							require.Contains(t, status.Warning, "reload failed")
						}
						effects = append(effects, "verify")
						return nil
					}},
				}
				require.NoError(t, r.execute(context.Background()))
				want := []string{"stop settings", "restart", "verify"}
				if action == update.ActionApply {
					want = []string{"stop settings", "apply", "restart", "verify"}
				}
				require.Equal(t, want, effects)
				r.sample()
				r.publish()
				status, err := h.LastRun()
				require.NoError(t, err)
				require.True(t, status.Success)
				require.True(t, status.Finished())
				require.Empty(t, status.Error)
				if fail {
					require.Contains(t, status.Warning, "configure node stop settings: reload failed")
					require.Contains(t, logs.String(), "continuing update and retrying next run")
				} else {
					require.Empty(t, status.Warning)
				}
			})
		}
	}
}
