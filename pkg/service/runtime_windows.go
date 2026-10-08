//go:build windows

package service

import (
	"os"
	"path/filepath"

	"github.com/stperic/zzrouter/pkg/observability/logger"
)

func runUnderServiceManager(run func() error) (bool, error) {
	if !IsWindowsService() {
		return false, nil
	}
	// SCM-launched processes have no console; if the early-stage log
	// routing in Execute() didn't fire (the spawner couldn't pre-set
	// ZZROUTER_LOG_FILE because SCM wraps argv before env), wire slog
	// into the rotating file backend at PROGRAMDATA so any runFunc
	// error has a durable artifact instead of disappearing into the
	// closed-stderr void. Failure here is silent — the service still
	// runs, operators just lose log fidelity.
	if os.Getenv("ZZROUTER_LOG_FILE") == "" {
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		path := filepath.Join(programData, "zzrouter", "logs", "zzrouter-node.log")
		_, _ = logger.UseFile(path, logger.FileBackendOptions{Compress: true})
	}
	return true, RunAsService(run)
}
