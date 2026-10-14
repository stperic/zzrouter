package host

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"testing"

	ps "github.com/mitchellh/go-ps"
	"github.com/stretchr/testify/require"
)

type tableProcess struct{ pid int }

func (p tableProcess) Pid() int           { return p.pid }
func (p tableProcess) PPid() int          { return 0 }
func (p tableProcess) Executable() string { return "zzrouter-node" }

func TestProcessTableReads(t *testing.T) {
	t.Run("enumeration", func(t *testing.T) {
		checkProcessTableReads(t, []ps.Process{tableProcess{pid: 42}})
	})
	t.Run("lookup", func(t *testing.T) {
		checkProcessTableReads[ps.Process](t, tableProcess{pid: 42})
	})
	t.Run("absent lookup", func(t *testing.T) {
		checkProcessTableReads[ps.Process](t, nil)
	})
}

func checkProcessTableReads[T any](t *testing.T, value T) {
	t.Helper()
	for _, tc := range []struct {
		name     string
		failures int
		err      error
	}{
		{"success", 0, nil},
		{"one size race", 1, syscall.ENOMEM},
		{"two size races", 2, syscall.ENOMEM},
		{"wrapped size race", 1, fmt.Errorf("sysctl: %w", syscall.ENOMEM)},
		{"exhaustion", 3, syscall.ENOMEM},
		{"permission", 1, syscall.EPERM},
		{"IO", 1, syscall.EIO},
		{"untyped message", 1, errors.New("cannot allocate memory")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := readProcessTable(func() (T, error) {
				calls++
				if calls <= tc.failures {
					var zero T
					return zero, tc.err
				}
				return value, nil
			})
			retry := runtime.GOOS == "darwin" && errors.Is(tc.err, syscall.ENOMEM)
			if tc.failures == 0 || retry && tc.failures < 3 {
				require.NoError(t, err)
				require.Equal(t, value, got)
				require.Equal(t, tc.failures+1, calls)
			} else {
				require.ErrorIs(t, err, tc.err)
				want := 1
				if retry {
					want = 3
				}
				require.Equal(t, want, calls)
			}
		})
	}
}

func TestProcessTableStopsOnDifferentError(t *testing.T) {
	calls := 0
	_, err := readProcessTable(func() (ps.Process, error) {
		calls++
		if calls == 1 {
			return nil, syscall.ENOMEM
		}
		return nil, syscall.EPERM
	})
	if runtime.GOOS == "darwin" {
		require.ErrorIs(t, err, syscall.EPERM)
		require.Equal(t, 2, calls)
	} else {
		require.ErrorIs(t, err, syscall.ENOMEM)
		require.Equal(t, 1, calls)
	}
}
