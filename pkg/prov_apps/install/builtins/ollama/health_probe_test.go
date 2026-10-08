package ollama

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHealthProbeCommands_NeverUseLocalhost is the regression from a
// real Windows install: Ollama binds IPv4 only, "localhost" resolves to
// ::1 first on Windows, and the probe burned its whole timeout on an
// address nothing was listening on. Every step of the install had
// worked; only the check that proves it failed.
func TestHealthProbeCommands_NeverUseLocalhost(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		poll, verify := healthProbeCommands(goos)
		for _, cmd := range []string{poll, verify} {
			assert.NotContains(t, cmd, "localhost", "%s probe must address 127.0.0.1 literally", goos)
			assert.Contains(t, cmd, "127.0.0.1:11434", "%s probe must reach the Ollama port", goos)
		}
	}
}

// TestHealthProbeCommands_WindowsReportsWhyItFailed keeps the failure
// branch from exiting mute. A bare `exit 1` reached the operator as
// "exit status 1: " with nothing after the colon.
func TestHealthProbeCommands_WindowsReportsWhyItFailed(t *testing.T) {
	poll, _ := healthProbeCommands("windows")

	assert.Contains(t, poll, "Error.WriteLine", "the failure path must say something on stderr")
	assert.Contains(t, poll, "$e", "and it must carry the last exception, not just a fixed string")
	assert.Contains(t, poll, "exit 1", "while still failing the step")
}

// TestHealthProbeCommands_WindowsQuotingSurvivesCmd guards the shape
// that was verified by hand on a Windows worker: the payload is wrapped
// in one pair of double quotes for `cmd /S /C`, so every quote inside
// it has to be a single quote.
func TestHealthProbeCommands_WindowsQuotingSurvivesCmd(t *testing.T) {
	poll, verify := healthProbeCommands("windows")

	for _, cmd := range []string{poll, verify} {
		body := cmd[strings.Index(cmd, `"`)+1 : strings.LastIndex(cmd, `"`)]
		assert.NotContains(t, body, `"`, "nested double quotes do not survive the cmd wrapper")
		assert.Equal(t, 2, strings.Count(cmd, `"`), "exactly one quoted -Command payload")
	}
}

// TestHealthProbeCommands_UnixKeepsTheRetryWindow pins the cold-start
// absorption: a service manager returns before the socket listens, so a
// single probe would hit connection-refused.
func TestHealthProbeCommands_UnixKeepsTheRetryWindow(t *testing.T) {
	poll, verify := healthProbeCommands("linux")

	assert.Contains(t, poll, "--retry 20")
	assert.NotContains(t, verify, "--retry", "verify stays a single fast probe for the pre-probe path")
}
