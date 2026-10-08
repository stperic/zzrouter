package update

import (
	"errors"
	"os"
	"testing"

	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSupervisor stands in for the OS service manager.
type fakeSupervisor struct {
	status      *service.Status
	statusErr   error
	restarts    bool
	restartsWhy string
}

func (f *fakeSupervisor) GetStatus() (*service.Status, error) { return f.status, f.statusErr }
func (f *fakeSupervisor) RestartsOnExit() (bool, string)      { return f.restarts, f.restartsWhy }

func TestSupervisorRestarter_RequestsExitWhenSupervised(t *testing.T) {
	requested := false
	r := &supervisorRestarter{
		supervisor: &fakeSupervisor{
			status:   &service.Status{Running: true, PID: 4711},
			restarts: true,
		},
		pid:     4711,
		request: func() { requested = true },
	}

	require.NoError(t, r.Restart())
	assert.True(t, requested, "a supervised node must ask the process to exit")
}

func TestSupervisorRestarter_RefusesWhenNothingWouldBringItBack(t *testing.T) {
	tests := []struct {
		name       string
		supervisor *fakeSupervisor
		pid        int
		wantReason string
	}{
		{
			name:       "service manager cannot be queried",
			supervisor: &fakeSupervisor{statusErr: errors.New("systemctl: command not found")},
			pid:        4711,
			wantReason: "systemctl: command not found",
		},
		{
			name:       "service is installed but not running us",
			supervisor: &fakeSupervisor{status: &service.Status{Running: false}, restarts: true},
			pid:        4711,
			wantReason: "not started by the service manager",
		},
		{
			name: "another process is the service",
			supervisor: &fakeSupervisor{
				status:   &service.Status{Running: true, PID: 22},
				restarts: true,
			},
			pid:        4711,
			wantReason: "not started by the service manager",
		},
		{
			name: "restart policy does not cover the exit",
			supervisor: &fakeSupervisor{
				status:      &service.Status{Running: true, PID: 4711},
				restarts:    false,
				restartsWhy: "zzrouter-node.service has Restart=no, which does not cover a failure exit",
			},
			pid:        4711,
			wantReason: "Restart=no",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requested := false
			r := &supervisorRestarter{
				supervisor: tt.supervisor,
				pid:        tt.pid,
				request:    func() { requested = true },
			}

			err := r.Restart()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrNoSupervisor)
			assert.Contains(t, err.Error(), tt.wantReason)
			assert.False(t, requested, "an unsupervised node must not exit — nothing would start it again")
		})
	}
}

func TestNewSupervisorRestarter_UsesThisProcess(t *testing.T) {
	r := newSupervisorRestarter()

	assert.Equal(t, os.Getpid(), r.pid)
	assert.NotNil(t, r.supervisor)
	assert.NotNil(t, r.request)
}

// TestRequestRestartExit_IsIdempotent closes the process-global restart
// channel. Nothing else in this package selects on it, and two applies
// racing to request the same exit must not panic on a double close.
func TestRequestRestartExit_IsIdempotent(t *testing.T) {
	select {
	case <-RestartRequested():
		t.Fatal("restart must not already be requested")
	default:
	}

	requestRestartExit()
	requestRestartExit()

	select {
	case <-RestartRequested():
	default:
		t.Fatal("requestRestartExit must close the channel")
	}
}
