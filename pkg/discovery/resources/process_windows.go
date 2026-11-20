package resources

import (
	"context"
	"errors"
	"unsafe"

	processinfo "github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"
)

func processParents(ctx context.Context) (map[int32]int32, error) {
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	parents := make(map[int32]int32)
	started := make(map[int32]int64)
	for err = windows.Process32First(handle, &entry); err == nil; err = windows.Process32Next(handle, &entry) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid := int32(entry.ProcessID)
		parents[pid] = int32(entry.ParentProcessID)
		started[pid] = processStarted(pid)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return verifyProcessParents(parents, started), nil
}

func sampleParent(ctx context.Context, pid int32) (int32, error) {
	parent, err := (&processinfo.Process{Pid: pid}).PpidWithContext(ctx)
	if err != nil || parent <= 0 {
		return parent, err
	}
	return verifiedParent(parent, processStarted(parent), processStarted(pid)), nil
}

func processStarted(pid int32) int64 {
	if pid <= 0 {
		return 0
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var started, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &started, &exited, &kernel, &user); err != nil || exited != (windows.Filetime{}) {
		return 0
	}
	return started.Nanoseconds()
}

func processAlive(_ context.Context, pid int32) (bool, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	switch {
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
		return false, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return true, nil
	case err != nil:
		return false, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	event, err := windows.WaitForSingleObject(handle, 0)
	return event == uint32(windows.WAIT_TIMEOUT), err
}
