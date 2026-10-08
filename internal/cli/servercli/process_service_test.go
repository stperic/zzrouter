package servercli

import (
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeServiceManager implements only the two methods the stop decision
// uses. The embedded interface supplies the rest and panics if the
// decision ever reaches for one, which is the assertion we want.
type fakeServiceManager struct {
	service.ServiceManager
	status    *service.Status
	statusErr error
	stopErr   error
	stopCalls int
}

func (f *fakeServiceManager) GetStatus() (*service.Status, error) { return f.status, f.statusErr }

func (f *fakeServiceManager) StopService() error {
	f.stopCalls++
	return f.stopErr
}

// TestStopServiceIfManaging_StopsTheService covers the case that made a
// node look like it would not stay stopped: killing a service's process
// only has the service manager restart it, on the same config.
func TestStopServiceIfManaging_StopsTheService(t *testing.T) {
	mgr := &fakeServiceManager{status: &service.Status{Running: true, PID: 4242}}

	stopped, err := stopServiceIfManaging(mgr, 4242)
	require.NoError(t, err)
	assert.True(t, stopped, "the caller must not go on to signal the process")
	assert.Equal(t, 1, mgr.stopCalls)
}

// TestStopServiceIfManaging_LeavesAnUnmanagedProcess keeps a node
// started by hand on the ordinary signal path, even while a service is
// installed and running as some other process.
func TestStopServiceIfManaging_LeavesAnUnmanagedProcess(t *testing.T) {
	mgr := &fakeServiceManager{status: &service.Status{Running: true, PID: 111}}

	stopped, err := stopServiceIfManaging(mgr, 4242)
	require.NoError(t, err)
	assert.False(t, stopped)
	assert.Zero(t, mgr.stopCalls)
}

// TestStopServiceIfManaging_NoServiceInstalled is the common case on a
// node started from a shell.
func TestStopServiceIfManaging_NoServiceInstalled(t *testing.T) {
	mgr := &fakeServiceManager{status: &service.Status{Error: "Service not installed"}}

	stopped, err := stopServiceIfManaging(mgr, 4242)
	require.NoError(t, err)
	assert.False(t, stopped)
	assert.Zero(t, mgr.stopCalls)
}

// TestStopServiceIfManaging_QueryFailureFallsThrough keeps stop working
// where the service manager cannot be queried at all.
func TestStopServiceIfManaging_QueryFailureFallsThrough(t *testing.T) {
	mgr := &fakeServiceManager{statusErr: errors.New("cannot connect to service manager")}

	stopped, err := stopServiceIfManaging(mgr, 4242)
	require.NoError(t, err)
	assert.False(t, stopped)
}

// TestStopServiceIfManaging_ReportsAPrivilegeFailure must not fall
// through to signalling: that would look like it worked, and the
// service would be back moments later.
func TestStopServiceIfManaging_ReportsAPrivilegeFailure(t *testing.T) {
	mgr := &fakeServiceManager{
		status:  &service.Status{Running: true, PID: 4242},
		stopErr: errors.New("access is denied"),
	}

	stopped, err := stopServiceIfManaging(mgr, 4242)
	require.Error(t, err)
	assert.False(t, stopped)
	assert.Contains(t, err.Error(), "access is denied", "the underlying refusal has to survive the wrap")
}

// TestStopServiceIfManaging_NilManager guards the platforms with no
// service manager at all.
func TestStopServiceIfManaging_NilManager(t *testing.T) {
	stopped, err := stopServiceIfManaging(nil, 4242)
	require.NoError(t, err)
	assert.False(t, stopped)
}
