package shared

import (
	"os"
	"runtime"
)

// BrowserAvailable avoids launching a browser on a remote or headless host.
// The caller always retains a URL that can be opened on the user's computer.
func BrowserAvailable() bool { return browserAvailable(runtime.GOOS, os.Getenv) }

func browserAvailable(platform string, getenv func(string) string) bool {
	for _, key := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if getenv(key) != "" {
			return false
		}
	}
	switch platform {
	case "darwin", "windows":
		return true
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris":
		return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
	default:
		return false
	}
}
