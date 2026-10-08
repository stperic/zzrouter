package service

// RunUnderServiceManager dispatches through the OS service runtime when
// the current process was launched by it. On Windows, when launched by
// SCM, this wraps run in svc.Run so the process answers SCM's
// StartPending/Running/StopPending protocol; SCM would otherwise mark
// the service as "failed to start" after ~30 seconds.
//
// On non-Windows platforms, and on Windows when launched from a shell
// rather than SCM, returns (false, nil) and the caller proceeds with
// the normal startup path.
func RunUnderServiceManager(run func() error) (handled bool, err error) {
	return runUnderServiceManager(run)
}
