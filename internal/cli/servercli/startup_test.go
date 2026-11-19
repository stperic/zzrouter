package servercli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stretchr/testify/require"
)

// The update restart signal is process-wide and one-shot, so that case runs in a child.
const runServeRestartChild = "ZZROUTER_RUNSERVE_RESTART_CHILD"

// A detached node discards stderr, so the end of runServe must reach the log
// once: at ERROR when construction refuses or a running server stops on its
// own (the path a role crossing takes through Start), at INFO when an update
// restart ends it on purpose. A refused access store's line names its reason
// and the operator's next action, and no path.
func TestRunServeLogsFinalErrorOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		serve   func(t *testing.T) error
		level   slog.Level
		want    string // the error text at ERROR, the restart reason at INFO
		refusal sqlstore.Reason
	}{
		{"construction", func(*testing.T) error {
			return runServe(&pkgConfig.NodeConfig{Auth: pkgConfig.AuthConfig{AdminKey: "too-short"}}, nil)
		}, slog.LevelError, "failed to create server", ""},
		{"store marker without its database", func(t *testing.T) error {
			cfg := runServeNode(t, freePort(t))
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ZZROUTER_CONFIG_DIR"), "access-store.json"), []byte(`{"version":1,"store_id":"lost"}`), 0o600))
			return runServe(cfg, nil)
		}, slog.LevelError, "failed to create server", sqlstore.Missing},
		{"store schema another tool changed", func(t *testing.T) error {
			cfg := runServeNode(t, freePort(t))
			dir := os.Getenv("ZZROUTER_CONFIG_DIR")
			store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, err)
			require.NoError(t, store.Close())
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "access", "access.sqlite")))
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "ANALYZE")
			require.NoError(t, errors.Join(err, db.Close()))
			return runServe(cfg, nil)
		}, slog.LevelError, "integrity_failed (its schema objects differ from this release's)", sqlstore.IntegrityFailed},
		{"unreadable store", func(t *testing.T) error {
			cfg := runServeNode(t, freePort(t))
			dir := filepath.Join(os.Getenv("ZZROUTER_CONFIG_DIR"), "access")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "access.sqlite"), []byte("not a database, just text long enough to have a header"), 0o600))
			return runServe(cfg, nil)
		}, slog.LevelError, "failed to create server", sqlstore.Unreadable},
		{"serving", func(t *testing.T) error {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer occupied.Close()
			return runServe(runServeNode(t, occupied.Addr().(*net.TCPAddr).Port), nil)
		}, slog.LevelError, "listen 127.0.0.1:", ""},
		{"update restart", serveUntilUpdateRestart, slog.LevelInfo, "update restart fixture", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.level == slog.LevelInfo && os.Getenv(runServeRestartChild) != "1" {
				binary, err := os.Executable()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				cmd := host.CommandContext(ctx, binary, "-test.run=^TestRunServeLogsFinalErrorOnce$/^update_restart$", "-test.v", "-test.count=1")
				cmd.Env = append(os.Environ(), runServeRestartChild+"=1")
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, string(out))
				require.Contains(t, string(out), "--- PASS: TestRunServeLogsFinalErrorOnce/update_restart")
				return
			}
			logs := &lockedBuffer{}
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			err := tc.serve(t)
			var exits []struct{ Level, Msg, Error, Reason string }
			for line := range strings.SplitSeq(logs.String(), "\n") {
				var record struct{ Level, Msg, Error, Reason string }
				if json.Unmarshal([]byte(line), &record) == nil && strings.HasPrefix(record.Msg, "zzRouter server exited") {
					exits = append(exits, record)
				}
			}
			require.Len(t, exits, 1)
			require.Equal(t, tc.level.String(), exits[0].Level)
			if tc.level == slog.LevelInfo {
				require.ErrorIs(t, err, errRestartForUpdate)
				require.Equal(t, tc.want, exits[0].Reason)
				return
			}
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, err.Error(), exits[0].Error)
			if tc.refusal != "" {
				var refusal *sqlstore.Refusal
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, tc.refusal, refusal.Reason)
				require.Contains(t, exits[0].Error, string(tc.refusal))
				require.Contains(t, exits[0].Error, refusal.NextAction())
				require.NotContains(t, exits[0].Error, os.Getenv("ZZROUTER_CONFIG_DIR"), "the line names no path")
			}
		})
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer probe.Close()
	return probe.Addr().(*net.TCPAddr).Port
}

// serveUntilUpdateRestart runs a real node and requests an update restart once it serves.
func serveUntilUpdateRestart(t *testing.T) error {
	port := freePort(t)
	done := make(chan error, 1)
	go func() { done <- runServe(runServeNode(t, port), nil) }()
	health := "http://127.0.0.1:" + strconv.Itoa(port) + "/health/live"
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, health, nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond)
	update.RequestExit("update restart fixture")
	select {
	case err := <-done:
		return err
	case <-time.After(time.Minute):
		t.Fatal("runServe did not stop after an update restart request")
		return nil
	}
}

// runServeNode gives one node a private home and install root.
func runServeNode(t *testing.T, port int) *pkgConfig.NodeConfig {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	t.Setenv("ZZROUTER_CONFIG_DIR", root)
	previous := fsroot.ProviderRootDir()
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previous) })
	return &pkgConfig.NodeConfig{
		Node:      pkgConfig.ServeConfig{Bind: "127.0.0.1", Port: port, Name: "final-error"},
		Providers: pkgConfig.ProvidersConfig{InstallDir: filepath.Join(root, "installed-providers")},
	}
}

// lockedBuffer collects log output that late goroutines may still write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
