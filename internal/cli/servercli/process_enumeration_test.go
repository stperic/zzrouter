package servercli

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"

	ps "github.com/mitchellh/go-ps"
	"github.com/stretchr/testify/require"
)

type enumeratedProcess struct{ pid int }

func (p enumeratedProcess) Pid() int           { return p.pid }
func (p enumeratedProcess) PPid() int          { return 0 }
func (p enumeratedProcess) Executable() string { return "zzrouter-node" }

func TestNodeProcessEnumerationDoesNotRetryReadErrors(t *testing.T) {
	for _, failure := range []error{syscall.ENOMEM, syscall.EPERM, syscall.EIO, errors.New("cannot allocate memory")} {
		calls := 0
		err := checkNodeProcesses(func() ([]ps.Process, error) {
			calls++
			return nil, failure
		})
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	}
}

func TestNodeProcessEnumerationStillRefusesExistingNode(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux additionally checks a live /proc command line; covered by serverproc_linux_test.go")
	}
	calls := 0
	err := checkNodeProcesses(func() ([]ps.Process, error) {
		calls++
		return []ps.Process{enumeratedProcess{pid: os.Getpid() + os.Getppid() + 1}}, nil
	})
	require.ErrorContains(t, err, "already running")
	require.Equal(t, 1, calls, "a running-node refusal must never be retried")
}
