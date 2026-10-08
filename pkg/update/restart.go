package update

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/service"
)

// ErrNoSupervisor reports that nothing would start the node again if it
// exited. An update that has already replaced the binary on disk is
// still applied when this comes back — only the switch to the new code
// is deferred to a manual restart.
var ErrNoSupervisor = errors.New("no service manager is supervising this process")

// Restarter hands the process back to whatever supervises it, so the
// node returns on the binary the update just installed.
type Restarter interface {
	// Restart requests the exit. It returns before the process is gone:
	// the drain runs on the goroutine that owns the process lifetime,
	// not on the caller's.
	Restart() error
}

// serviceSupervisor is the slice of service.ServiceManager a restart
// needs: which process the supervisor is currently running, and whether
// it would start the node again after an exit.
type serviceSupervisor interface {
	GetStatus() (*service.Status, error)
	RestartsOnExit() (bool, string)
}

// supervisorRestarter exits the process and lets systemd, launchd or
// the Windows SCM start it again.
//
// It does not ask the supervisor to restart us (`systemctl restart` and
// friends): that needs privileges the node does not have when it runs
// as its own service user, and both the launchd and the SCM sequence
// kill this process midway through, leaving nothing to run the second
// half. Exiting is privilege-free, and the supervisor owns the ordering.
type supervisorRestarter struct {
	supervisor serviceSupervisor
	pid        int
	request    func()
}

func newSupervisorRestarter() *supervisorRestarter {
	return &supervisorRestarter{
		supervisor: service.NewServiceManager(),
		pid:        os.Getpid(),
		request:    requestRestartExit,
	}
}

// Restart verifies both halves of "something will bring us back" before
// exiting: the supervisor is running *this* process, and its restart
// policy covers a failure exit.
func (r *supervisorRestarter) Restart() error {
	if err := r.canRestart(); err != nil {
		return err
	}
	r.request()
	return nil
}

// canRestart is the "would exiting bring us back" question on its own,
// so callers that exit for reasons other than an install can ask it
// before they do. Exiting without asking is how a node goes down and
// stays down.
func (r *supervisorRestarter) canRestart() error {
	status, err := r.supervisor.GetStatus()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoSupervisor, err)
	}
	if status == nil || !status.Running || status.PID != r.pid {
		return fmt.Errorf("%w: the node was not started by the service manager", ErrNoSupervisor)
	}
	if restarts, why := r.supervisor.RestartsOnExit(); !restarts {
		return fmt.Errorf("%w: %s", ErrNoSupervisor, why)
	}
	return nil
}

// CanRestart reports whether exiting now would have this process
// started again, wrapping ErrNoSupervisor with the reason when it would
// not.
//
// A rollback has the same stake as an install: it changes the binary on
// disk and is only useful if something runs it. Exiting to apply one on
// a node nothing supervises trades a bad version for no version.
func CanRestart() error { return newSupervisorRestarter().canRestart() }

// restartExit is process-global on purpose: "this process is about to
// exit" is not a per-scheduler fact, and the serve loop that acts on it
// has no handle to whatever raised it.
var (
	restartExit     = make(chan struct{})
	restartExitOnce sync.Once
	restartReason   atomic.Value
)

// RestartRequested is closed once the process needs to exit so its
// supervisor starts it again: after an update installs, and after an
// update that never came up healthy is rolled back. Whoever owns the
// process lifetime selects on it, drains gracefully, and exits with a
// failure status — the exit that every supported supervisor's restart
// policy keys on. Which binary comes back is decided by what is on disk
// at that point, not by this signal.
func RestartRequested() <-chan struct{} { return restartExit }

// RestartReason describes why the exit was requested, for the operator
// reading the log line that precedes it.
func RestartReason() string {
	reason, _ := restartReason.Load().(string)
	return reason
}

// RequestExit asks the process to shut down and be restarted. Safe to
// call more than once; the first reason is the one reported, since it
// is the one that started the shutdown.
func RequestExit(reason string) {
	restartExitOnce.Do(func() {
		restartReason.Store(reason)
		close(restartExit)
	})
}

func requestRestartExit() { RequestExit("an update was installed") }
