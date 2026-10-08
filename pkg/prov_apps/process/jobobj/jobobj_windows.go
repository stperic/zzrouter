//go:build windows

// Package jobobj provides a thin wrapper around Windows Job Objects for
// managing provider process trees. Each provider instance gets its own
// Job Object, giving zzrouter-node two superpowers:
//
//   - Atomic tree kill: TerminateJobObject kills the launcher AND all its
//     descendants in one kernel call — no recursive PID walk, no missed
//     grandchildren.
//   - Crash orphan cleanup: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE tells the
//     kernel to kill everything in the job when the last handle closes.
//     If zzrouter-node crashes, the handle is closed by the OS, and all
//     provider processes die automatically. Zero GPU VRAM leaks.
//
// This package is Windows-only. Callers use platform-split files
// (platform_windows.go / platform_unix.go) to gate imports.
package jobobj

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job wraps a Windows Job Object handle.
type Job struct {
	handle windows.Handle
}

// Create creates a new unnamed Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. When the last handle to the job
// is closed (including on parent crash), Windows kills all processes in
// the job.
func Create() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// Set KILL_ON_JOB_CLOSE so orphan cleanup is automatic.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return &Job{handle: h}, nil
}

// AssignProcess adds a process to this Job Object by PID. The process
// must not already belong to another job (Windows limitation prior to
// Windows 8 / Server 2012; nested jobs are supported on newer versions).
func (j *Job) AssignProcess(pid int) error {
	if j == nil || j.handle == 0 {
		return fmt.Errorf("invalid job handle")
	}

	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(ph) //nolint:errcheck // best-effort cleanup in defer

	if err := windows.AssignProcessToJobObject(j.handle, ph); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// Terminate kills all processes in the job with the given exit code.
// This is the force-kill path — no graceful shutdown, just death.
func (j *Job) Terminate(exitCode uint32) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	return windows.TerminateJobObject(j.handle, exitCode)
}

// Handle returns the raw Windows handle for storage as uintptr on
// cross-platform structs. The caller must call Close() when done.
func (j *Job) Handle() uintptr {
	if j == nil {
		return 0
	}
	return uintptr(j.handle)
}

// Close releases the Job Object handle. If KILL_ON_JOB_CLOSE is set
// and processes are still assigned, the kernel kills them.
func (j *Job) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	return err
}

// FromHandle wraps an existing handle (e.g., retrieved from Instance
// storage as uintptr) back into a Job. The caller owns the handle and
// must call Close().
func FromHandle(h uintptr) *Job {
	if h == 0 {
		return nil
	}
	return &Job{handle: windows.Handle(h)}
}
