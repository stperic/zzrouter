package servercli

import (
	"fmt"
	"runtime"

	"github.com/stperic/zzrouter/pkg/service"
)

// runLoginStart reuses the service manager for an explicit user LaunchAgent.
func runLoginStart(manager service.ServiceManager) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("--start-at-login is available on macOS; use the platform service installation on Windows or Linux")
	}
	if manager.IsPrivileged() {
		return fmt.Errorf("start the LaunchAgent as your own user, without sudo")
	}
	if err := manager.SetupDirectories(); err != nil {
		return err
	}
	if _, err := manager.GenerateAPIKey(); err != nil {
		return err
	}
	if !manager.IsFirstRun() {
		_ = manager.StopService()
	}
	if err := manager.InstallService(); err != nil {
		return err
	}
	return manager.StartService()
}
