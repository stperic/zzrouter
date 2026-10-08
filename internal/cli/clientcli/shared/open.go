package shared

import (
	"fmt"
	"runtime"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
)

// openLaunchGrace is how long OpenFile waits for the launcher to fail.
// Handlers hand off and exit in milliseconds, so an exit inside this
// window is a real error ("no application knows this type", no $DISPLAY)
// rather than the user closing the app.
const openLaunchGrace = 300 * time.Millisecond

// OpenFile hands a path to the desktop handler registered for its type,
// the same way double-clicking it would. It reports an immediate launch
// failure but does not wait for the application to be closed.
func OpenFile(path string) error {
	name, args := openCommand(path)
	if name == "" {
		return fmt.Errorf("no file opener known for %s", runtime.GOOS)
	}

	cmd := host.Command(name, args...)
	detachProcess(cmd) // survive the TUI's terminal signals
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	// Always reap: without a Wait the launcher lingers as a zombie for
	// the lifetime of the TUI.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	case <-time.After(openLaunchGrace):
		// Still running: it took over the file, and the goroutine above
		// still owns the eventual Wait.
		return nil
	}
}

// openCommand returns the platform's file-association launcher. Windows
// goes through rundll32 rather than `cmd /c start`, which is a shell
// builtin that treats a quoted first argument as a window title.
// ShellExec_RunDLL is used over FileProtocolHandler because the latter
// URL-decodes its argument, mangling any path containing % or #.
func openCommand(path string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{path}
	case "windows":
		return "rundll32", []string{"shell32.dll,ShellExec_RunDLL", path}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris":
		return "xdg-open", []string{path}
	}
	return "", nil
}
