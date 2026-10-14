package host

import (
	"errors"
	"runtime"
	"syscall"

	ps "github.com/mitchellh/go-ps"
)

// Processes reads the host process table, recovering transient Darwin size races.
func Processes() ([]ps.Process, error) {
	return readProcessTable(ps.Processes)
}

// FindProcess looks up a PID, returning nil without an error when it is absent.
func FindProcess(pid int) (ps.Process, error) {
	return readProcessTable(func() (ps.Process, error) { return ps.FindProcess(pid) })
}

func readProcessTable[T any](read func() (T, error)) (T, error) {
	result, err := read()
	// Darwin sizes then reads KERN_PROC_ALL; table growth can invalidate that size.
	const processTableAttempts = 3
	for attempt := 1; attempt < processTableAttempts && runtime.GOOS == "darwin" && errors.Is(err, syscall.ENOMEM); attempt++ {
		result, err = read()
	}
	return result, err
}
