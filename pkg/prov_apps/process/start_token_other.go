//go:build !linux

package process

import "github.com/shirou/gopsutil/v4/process"

// StartToken reads an opaque, exec-stable identity, compared exactly on this platform.
func StartToken(pid int) (int64, error) {
	return (&process.Process{Pid: int32(pid)}).CreateTime()
}
