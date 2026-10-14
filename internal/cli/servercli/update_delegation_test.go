package servercli

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

type updateCLIFile struct {
	Mode fs.FileMode
	Data string
	Time time.Time
}

func updateCLITree(t *testing.T, root string) map[string]updateCLIFile {
	t.Helper()
	files := make(map[string]updateCLIFile)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := updateCLIFile{Mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			file.Data, err = os.Readlink(path)
		case !entry.IsDir():
			var data []byte
			data, err = os.ReadFile(path)
			file.Data, file.Time = string(data), info.ModTime()
		}
		files[rel] = file
		return err
	}))
	return files
}

// The CLI consumer supplies the real handoff; package update tests exercise
// the concrete scheduler's request factory and managed-install decision.
type cliRequester struct {
	h   *update.Handoff
	err error
}

func (s cliRequester) Delegated() *update.Handoff { return s.h }
func (s cliRequester) RequestPrivileged(action update.RequestAction, by string) error {
	if s.err != nil {
		return s.err
	}
	return s.h.Submit(&update.Request{Action: action, RequestedBy: by})
}

func cliRequest(t *testing.T, h *update.Handoff, submitErr error, uid int, action update.RequestAction, run func(context.Context, *update.Handoff, *update.Request) error) (bool, string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	handled, err := requestCLIUpdate(cmd, cliRequester{h: h, err: submitErr}, action, uid, run)
	return handled, out.String(), err
}

func updateCLIFixture(t *testing.T) (string, *update.Handoff, *update.Installer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed layout uses Unix symlinks")
	}
	root := t.TempDir()
	install := filepath.Join(root, "install")
	for _, ver := range []string{"1.0.0", "2.0.0"} {
		dir := filepath.Join(install, "versions", ver, "bin")
		require.NoError(t, os.MkdirAll(dir, 0755))
		for _, name := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(ver+" "+name), 0755))
		}
	}
	bin := filepath.Join(install, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	for _, name := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
		require.NoError(t, os.Symlink(filepath.Join("..", "versions", "2.0.0", "bin", name), filepath.Join(bin, name)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(install, "state.json"), []byte(`{"previous":["1.0.0"]}`), 0644))
	h := update.NewHandoff(filepath.Join(root, "requests"), filepath.Join(root, "status"))
	require.NoError(t, os.MkdirAll(h.RequestDir(), 0750))
	require.NoError(t, os.MkdirAll(filepath.Dir(h.StatusPath()), 0755))
	i := update.NewInstaller(filepath.Join(root, "status", "backups"), 2)
	i.SetCurrentExecutable(filepath.Join(install, "versions", "2.0.0", "bin", "zzrouter-node"))
	return root, h, i
}

func TestUpdateCLIManagedSubmissionChangesOnlyRequest(t *testing.T) {
	for _, action := range []update.RequestAction{update.ActionApply, update.ActionRollback} {
		t.Run(string(action), func(t *testing.T) {
			root, h, _ := updateCLIFixture(t)
			before := updateCLITree(t, root)
			handled, out, err := cliRequest(t, h, nil, 1000, action, nil)
			require.True(t, handled)
			require.NoError(t, err)
			require.Contains(t, out, "Requested "+string(action))
			require.Contains(t, out, h.RequestPath())
			require.Contains(t, out, "update status --json")
			require.Contains(t, out, "journalctl -u zzrouter-update.service -f")
			req, err := h.Pending()
			require.NoError(t, err)
			require.NotNil(t, req)
			require.Equal(t, action, req.Action)
			require.Equal(t, "cli", req.RequestedBy)
			require.Empty(t, req.TargetVersion)
			require.False(t, req.RequestedAt.IsZero())
			after := updateCLITree(t, root)
			delete(after, filepath.Join("requests", "request.json"))
			require.Equal(t, before, after)
		})
	}
}

func TestUpdateCLISubmissionFailureIsHandledWithoutChanges(t *testing.T) {
	for _, action := range []update.RequestAction{update.ActionApply, update.ActionRollback} {
		for _, pending := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pending=%v", action, pending), func(t *testing.T) {
				root, h, _ := updateCLIFixture(t)
				var injected error
				if pending {
					require.NoError(t, h.Submit(&update.Request{Action: update.ActionCheck}))
				} else {
					injected = fmt.Errorf("submit request: %w", fs.ErrPermission)
				}
				before := updateCLITree(t, root)
				handled, out, err := cliRequest(t, h, injected, 1000, action, nil)
				require.True(t, handled, "failure must not fall through to direct mutation")
				if pending {
					require.ErrorIs(t, err, update.ErrApplyInFlight)
				} else {
					require.ErrorIs(t, err, fs.ErrPermission)
					require.ErrorContains(t, err, "service account")
					require.ErrorContains(t, err, "or as root")
				}
				require.NotContains(t, out, "Requested")
				require.Equal(t, before, updateCLITree(t, root))
			})
		}
	}
}

func TestUpdateCLIFilesystemPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("filesystem permission denial requires an unprivileged process")
	}
	root, h, _ := updateCLIFixture(t)
	require.NoError(t, os.Chmod(h.RequestDir(), 0500))
	t.Cleanup(func() { _ = os.Chmod(h.RequestDir(), 0750) })
	before := updateCLITree(t, root)
	handled, _, err := cliRequest(t, h, nil, os.Geteuid(), update.ActionRollback, nil)
	require.True(t, handled)
	require.ErrorIs(t, err, fs.ErrPermission)
	require.ErrorContains(t, err, "service account")
	require.ErrorContains(t, err, "or as root")
	require.Equal(t, before, updateCLITree(t, root))
}

func TestUpdateCLIUnmanagedDoesNotDispatch(t *testing.T) {
	for _, uid := range []int{0, 1000} {
		handled, out, err := cliRequest(t, nil, nil, uid, update.ActionRollback, func(context.Context, *update.Handoff, *update.Request) error {
			t.Fatal("root executor called on unmanaged install")
			return nil
		})
		require.False(t, handled)
		require.NoError(t, err)
		require.Empty(t, out)
	}
}

func fakeSystemctl(t *testing.T, state string, fail bool) string {
	t.Helper()
	bin := t.TempDir()
	path := filepath.Join(bin, "systemctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\nprintf '%s\\n' '" + state + "'\n"
	if fail {
		script += "exit 1\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(script), 0755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestRootCLIRefusesBusyOrUnknownUpdater(t *testing.T) {
	for _, state := range []string{"active", "activating", "deactivating", "reloading", "", "unknown", "query error", "pending", "published run"} {
		t.Run(state, func(t *testing.T) {
			root, h, _ := updateCLIFixture(t)
			answer := state
			if state == "pending" {
				require.NoError(t, h.Submit(&update.Request{Action: update.ActionCheck}))
				answer = "inactive"
			}
			if state == "published run" {
				require.NoError(t, h.Publish(&update.RunStatus{Action: update.ActionApply, State: update.StateApplying}))
				answer = "inactive"
			}
			path := fakeSystemctl(t, answer, state == "query error")
			before := updateCLITree(t, root)
			handled, _, err := cliRequest(t, h, nil, 0, update.ActionRollback, runCLIPrivilegedUpdate)
			require.True(t, handled)
			require.Error(t, err)
			require.Equal(t, before, updateCLITree(t, root))
			args, err := os.ReadFile(path + ".args")
			require.NoError(t, err)
			require.Contains(t, strings.Fields(string(args)), "zzrouter-update.service")
		})
	}
}

type updateTestService struct {
	service.ServiceManager
	restart func() error
}

func (m updateTestService) RestartService() error { return m.restart() }

type updateTestProbe struct{ check func() error }

func (p updateTestProbe) waitUntilServing(<-chan struct{}) error { return p.check() }

type installerScheduler struct {
	*update.Scheduler
	installer *update.Installer
}

func (s installerScheduler) Rollback(context.Context) (*update.InstallResult, error) {
	return s.installer.Rollback()
}

func TestRootCLIRollbackRestartsVerifiesAndPublishes(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("healthy=%v", healthy), func(t *testing.T) {
			root, h, installer := updateCLIFixture(t)
			fakeSystemctl(t, "inactive", false)
			before := updateCLITree(t, h.RequestDir())
			var effects []string
			handled, out, err := cliRequest(t, h, nil, 0, update.ActionRollback, func(ctx context.Context, h *update.Handoff, req *update.Request) error {
				if err := checkPrivilegedUpdaterIdle(ctx, h); err != nil {
					return err
				}
				s := update.NewSchedulerIn(filepath.Join(root, "status"), &config.UpdateConfig{}, clock.System())
				defer s.Stop()
				r := &privilegedRun{handoff: h, req: req, sched: installerScheduler{Scheduler: s, installer: installer}, restart: &deferredRestart{},
					manager: updateTestService{restart: func() error {
						data, err := os.ReadFile(filepath.Join(root, "install", "bin", "zzrouter-node"))
						require.NoError(t, err)
						require.Equal(t, "1.0.0 zzrouter-node", string(data))
						effects = append(effects, "restart")
						return nil
					}},
					probe: updateTestProbe{check: func() error {
						effects = append(effects, "health")
						if !healthy {
							return fmt.Errorf("node did not serve")
						}
						return nil
					}},
				}
				return r.execute(ctx)
			})
			require.True(t, handled)
			if healthy {
				require.NoError(t, err)
				require.Contains(t, out, "Privileged rollback completed")
			} else {
				require.ErrorContains(t, err, "node did not serve")
				require.NotContains(t, out, "completed")
			}
			require.Equal(t, []string{"restart", "health"}, effects)
			require.Equal(t, before, updateCLITree(t, h.RequestDir()))
			status, err := h.LastRun()
			require.NoError(t, err)
			require.NotNil(t, status)
			require.True(t, status.Finished())
			require.Equal(t, healthy, status.Success)
			require.Equal(t, "1.0.0", status.ToVersion)
		})
	}
}

func TestRootCLICommonExecutorPublishesFailureWithoutRequest(t *testing.T) {
	t.Setenv(allowConfigSourceEnv, "1")
	_, h, _ := updateCLIFixture(t)
	fakeSystemctl(t, "failed", false)
	before := updateCLITree(t, h.RequestDir())
	handled, _, err := cliRequest(t, h, nil, 0, update.ActionApply, func(ctx context.Context, h *update.Handoff, req *update.Request) error {
		if err := checkPrivilegedUpdaterIdle(ctx, h); err != nil {
			return err
		}
		return executeUpdateRequest(ctx, h, req, false, func() (*config.NodeConfig, error) {
			return &config.NodeConfig{Update: config.UpdateConfig{Source: &config.UpdateSourceConfig{Owner: "fixture", Repo: "fixture", APIBaseURL: "http://127.0.0.1:1", AllowedHosts: []string{"127.0.0.1"}}}}, nil
		})
	})
	require.True(t, handled)
	require.ErrorContains(t, err, "failed to check for updates")
	require.Equal(t, before, updateCLITree(t, h.RequestDir()))
	status, err := h.LastRun()
	require.NoError(t, err)
	require.NotNil(t, status)
	require.True(t, status.Finished())
	require.False(t, status.Success)
	require.Equal(t, update.ActionApply, status.Action)
	require.NotEmpty(t, status.FromVersion, "outer failure reporting must retain the completed run's details")
}

func TestRootCLISourcePolicyPrecedesValidation(t *testing.T) {
	t.Setenv(allowConfigSourceEnv, "")
	_, h, _ := updateCLIFixture(t)
	cfg := &config.NodeConfig{Update: config.UpdateConfig{Channel: "invalid", Source: &config.UpdateSourceConfig{Owner: "untrusted partial source"}}}
	err := executeUpdateRequest(t.Context(), h, &update.Request{Action: update.ActionApply, RequestedBy: "cli"}, false, func() (*config.NodeConfig, error) { return cfg, nil })
	require.ErrorContains(t, err, "invalid update channel")
	require.Nil(t, cfg.Update.Source)
}

func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	cmd := NewUpdateCmd()
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.Execute()
}

func TestUpdateCLIConfigValidationAndReads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	cfg := filepath.Join(root, "node.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("update:\n  source:\n    owner: untrusted\n"), 0600))
	for _, args := range [][]string{{"apply"}, {"rollback"}, {"rollback", "--list"}, {"check"}, {"status"}, {"history"}} {
		require.ErrorContains(t, runCLI(t, args...), "invalid update config")
	}
	require.NoError(t, os.WriteFile(cfg, []byte("update:\n  source:\n    owner: fixture\n    repo: fixture\n    api_base_url: http://127.0.0.1:1\n    allowed_hosts: [127.0.0.1]\n"), 0600))
	before := updateCLITree(t, root)
	for _, args := range [][]string{{"rollback", "--list"}, {"status", "--json"}, {"history"}} {
		require.NoError(t, runCLI(t, args...))
	}
	require.Equal(t, before, updateCLITree(t, root))
	require.ErrorContains(t, runCLI(t, "apply"), "failed to check for updates")
	require.ErrorContains(t, runCLI(t, "check"), "check for updates")
	require.Equal(t, before, updateCLITree(t, root))
}

func TestUpdateCLICheckDoesNotAutoApply(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"tag_name":"v99.99.99","assets":[{"name":"zzrouter-%s-%s.tar.gz","size":1,"browser_download_url":"https://example.invalid/archive"}]}]`, runtime.GOOS, runtime.GOARCH)
	}))
	defer feed.Close()
	require.NoError(t, os.WriteFile(filepath.Join(root, "node.yaml"), []byte("update:\n  source:\n    owner: fixture\n    repo: fixture\n    api_base_url: "+feed.URL+"\n    allowed_hosts: [127.0.0.1]\n"), 0600))
	before := updateCLITree(t, root)
	require.NoError(t, runCLI(t, "check"))
	require.Equal(t, before, updateCLITree(t, root))
}

var _ updateRequester = (*update.Scheduler)(nil)
