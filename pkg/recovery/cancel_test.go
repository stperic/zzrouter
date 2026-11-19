package recovery

import (
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// checkpoints counts the checks of its Err and cancels at the nth, so an
// operation can be cancelled at each of its cancellation points in turn.
type checkpoints struct {
	context.Context
	cancel      context.CancelFunc
	left, count atomic.Int64
}

// cancelAt cancels at the nth check; n below 1 never cancels.
func cancelAt(t *testing.T, n int64) *checkpoints {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c := &checkpoints{Context: ctx, cancel: cancel}
	c.left.Store(n)
	return c
}

func (c *checkpoints) Err() error {
	c.count.Add(1)
	if c.left.Add(-1) == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

// spread returns up to eight cancellation points across a run of total checks.
func spread(total int64) []int64 {
	var points []int64
	for i := int64(0); i < 8; i++ {
		if n := 1 + i*(total-1)/7; len(points) == 0 || points[len(points)-1] != n {
			points = append(points, n)
		}
	}
	return points
}

// noTemporary requires no .tmp entry anywhere under dir.
func noTemporary(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		require.False(t, strings.HasSuffix(path, ".tmp"), "a temporary is left: %s", path)
		return err
	}))
}

// TestRecoveryCancellationLeavesNothing cancels backup, check and restore at
// points spread over a whole run, including inside a file's copy. A cancelled
// run fails with the context's error and leaves no temporary anywhere, no
// archive and no key; a restore leaves nothing, or, once staging began, its
// staging directory marked INCOMPLETE.
func TestRecoveryCancellationLeavesNothing(t *testing.T) {
	dir, _, _ := recoveryFixture(t)
	large := make([]byte, 1<<20) // sixteen copy chunks
	_, err := rand.Read(large)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large.bin"), large, 0o600))
	temp := privateTemp(t)
	outputs := func() (string, string) {
		return filepath.Join(t.TempDir(), "backup.tar.gz"), filepath.Join(t.TempDir(), "credentials.key")
	}
	var archive, key string
	runs := map[string]func(ctx context.Context) (left []string, err error){
		"backup": func(ctx context.Context) ([]string, error) {
			output, keyOutput := outputs()
			_, err := Backup(ctx, dir, output, keyOutput)
			if err == nil {
				archive, key = output, keyOutput
			}
			return []string{filepath.Dir(output), filepath.Dir(keyOutput)}, err
		},
		"check": func(ctx context.Context) ([]string, error) {
			_, err := Check(ctx, archive, key)
			return nil, err
		},
		"restore": func(ctx context.Context) ([]string, error) {
			destination := filepath.Join(t.TempDir(), "restored")
			_, err := Restore(ctx, archive, key, destination)
			if err != nil {
				noTemporary(t, filepath.Dir(destination))
				if _, statErr := os.Stat(destination); statErr == nil {
					require.FileExists(t, filepath.Join(destination, "INCOMPLETE"))
				}
			}
			return nil, err
		},
	}
	for _, name := range []string{"backup", "check", "restore"} {
		t.Run(name, func(t *testing.T) {
			counter := cancelAt(t, 0)
			_, err := runs[name](counter)
			require.NoError(t, err)
			cancelled := 0
			for _, n := range spread(counter.count.Load()) {
				left, err := runs[name](cancelAt(t, n))
				emptyDir(t, temp, "nothing in the system's temporary directory")
				if err == nil {
					continue // cancelled after its last cancellation point
				}
				cancelled++
				require.ErrorIs(t, err, context.Canceled, "cancelled at check %d of %d", n, counter.count.Load())
				for _, dir := range left {
					emptyDir(t, dir, "no output and no temporary")
				}
			}
			t.Logf("%d checks; cancelled %d of %v", counter.count.Load(), cancelled, spread(counter.count.Load()))
			require.Positive(t, cancelled)
		})
	}
}

// onFirstCheck runs fn when its Err is first checked.
type onFirstCheck struct {
	context.Context
	once sync.Once
	fn   func()
}

func (c *onFirstCheck) Err() error {
	c.once.Do(c.fn)
	return c.Context.Err()
}

// TestRecoveryRestoreRefusesAnArchiveReplacedBetweenPasses: an archive that
// another valid archive replaces after the check pass opened it is refused
// before anything is prepared, though both archives pass on their own.
func TestRecoveryRestoreRefusesAnArchiveReplacedBetweenPasses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to replace an open file")
	}
	dir, output, keyOutput := recoveryFixture(t)
	_, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	other, otherKey := filepath.Join(t.TempDir(), "other.tar.gz"), filepath.Join(t.TempDir(), "other.key")
	_, err = Backup(t.Context(), dir, other, otherKey)
	require.NoError(t, err)
	_, err = Check(t.Context(), other, keyOutput)
	require.NoError(t, err, "the replacement passes its own check")
	destination := filepath.Join(t.TempDir(), "restored")
	ctx := &onFirstCheck{Context: t.Context(), fn: func() { require.NoError(t, os.Rename(other, output)) }}
	_, err = Restore(ctx, output, keyOutput, destination)
	require.ErrorContains(t, err, "changed while it was restored")
	require.FileExists(t, filepath.Join(destination, "INCOMPLETE"))
	require.NoFileExists(t, filepath.Join(destination, filepath.FromSlash(imageName)))
}
