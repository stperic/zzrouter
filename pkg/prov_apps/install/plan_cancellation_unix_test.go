//go:build unix

package install

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gprocess "github.com/shirou/gopsutil/v4/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanCancellationStopsDescendantsAndReleasesLock(t *testing.T) {
	for _, mode := range []string{"buffered", "streaming", "pty"} {
		t.Run(mode, func(t *testing.T) {
			fsroot.SetProviderRootOverride(t.TempDir())
			t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			step := Step{Number: 1, Command: "(trap '' HUP; sleep 30) & echo $! > " + fsroot.ShellQuote(pidFile) + "; wait", Timeout: time.Minute}
			if mode != "buffered" {
				step.StdoutLine = func(string) {}
			}
			step.UsePTY = mode == "pty"
			plan := Plan{Provider: "test-cancel", Steps: []Step{step}}
			lock := fsroot.NewLockFile(plan.Provider)
			require.NoError(t, lock.Lock())
			done := make(chan error, 1)
			go func() { err := plan.Execute(ctx, nil); lock.Unlock(); done <- err }()
			var pid int32
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				parsed, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
				pid = int32(parsed)
				return err == nil && pid > 1
			}, 3*time.Second, 10*time.Millisecond)
			t.Cleanup(func() {
				if p, err := gprocess.NewProcess(pid); err == nil {
					_ = p.Kill()
				}
			})
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(4 * time.Second):
				t.Fatal("installer command outlived cancellation")
			}
			assert.Eventually(t, func() bool {
				p, err := gprocess.NewProcess(pid)
				if err != nil {
					return true
				}
				status, err := p.Status()
				if err != nil {
					return true
				}
				for _, s := range status {
					if s == "zombie" {
						return true
					}
				}
				return !process.ProcessExists(pid)
			}, 3*time.Second, 10*time.Millisecond)
			replacement := fsroot.NewLockFile(plan.Provider)
			require.NoError(t, replacement.Lock())
			replacement.Unlock()
		})
	}
}
