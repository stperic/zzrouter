package ollama

import (
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowsSteps builds the Windows install steps for assertion.
func windowsSteps(t *testing.T) []install.Step {
	t.Helper()
	plan := &install.Plan{}
	New(nil).buildWindowsSteps(plan, "https://example.test/ollama-windows-amd64.zip", "ollama-windows-amd64.zip", "0.32.15")
	require.NotEmpty(t, plan.Steps)
	return plan.Steps
}

// stepIndex returns the position of the first step whose description
// contains want.
func stepIndex(t *testing.T, steps []install.Step, want string) int {
	t.Helper()
	for i, s := range steps {
		if strings.Contains(s.Description, want) {
			return i
		}
	}
	t.Fatalf("no step described as %q", want)
	return -1
}

// TestWindowsStopsTheServerBeforeActivating is the regression from a
// real install. Windows renames a directory holding a running .exe but
// refuses to delete one, so leaving the daemon up until the start step
// made the activation's cleanup of the displaced tree fail, failing an
// install whose every other step had succeeded. Linux had the stop
// before its swap all along; only this platform did not.
func TestWindowsStopsTheServerBeforeActivating(t *testing.T) {
	steps := windowsSteps(t)

	stop := stepIndex(t, steps, "Stop the running Ollama server")
	activate := stepIndex(t, steps, "Activate the staged install")
	start := stepIndex(t, steps, "Start Ollama")

	assert.Less(t, stop, activate, "the swap cannot clean up a tree the daemon still holds open")
	assert.Less(t, activate, start, "and the new binary starts only once it is in place")
}

// TestWindowsStartStepOnlyStarts keeps the stop from drifting back into
// the start step, where it ran too late to help the activation.
func TestWindowsStartStepOnlyStarts(t *testing.T) {
	steps := windowsSteps(t)
	start := steps[stepIndex(t, steps, "Start Ollama")]

	assert.NotContains(t, start.Command, "Stop-Process",
		"stopping belongs to its own step, before the swap")
	assert.Equal(t, install.ServiceStart, start.ServiceAction)
	assert.Empty(t, start.Command)
}

// TestWindowsStopTargetsOnlyOurBinary holds the same line uninstall
// does: an Ollama the operator installed elsewhere is not ours to kill.
func TestWindowsStopTargetsOnlyOurBinary(t *testing.T) {
	steps := windowsSteps(t)
	stop := steps[stepIndex(t, steps, "Stop the running Ollama server")]

	assert.Equal(t, install.ServiceStop, stop.ServiceAction)
	assert.Empty(t, stop.Command, "the runtime owner identifies the canonical binary and arguments")
}

// TestWindowsStepNumbersAreSequential guards the renumbering: steps are
// reported to operators and addressed individually by the single-step
// executor, so a gap or duplicate misdirects both.
func TestWindowsStepNumbersAreSequential(t *testing.T) {
	steps := windowsSteps(t)
	for i, s := range steps {
		assert.Equal(t, i+1, s.Number, "step %d is numbered %d", i+1, s.Number)
	}
}

// TestWindowsCommandsUsePlainPipes guards every Windows command in the
// plan against the caret-escaped pipe.
//
// These run as `cmd /S /C "<command>"`, which strips the outer quote
// pair and passes the rest through verbatim, so `^` is not an escape
// there: it reaches PowerShell as an argument and breaks the pipeline.
// It broke it silently, too, because the exit code came from the
// statement after the semicolon, so a stop step that stopped nothing
// and a safety check that checked nothing both reported success.
// Measured on a Windows 11 worker through that exact wrapper: `|`
// returns the PID, `^|` fails with a positional-parameter error.
func TestWindowsCommandsUsePlainPipes(t *testing.T) {
	for _, s := range windowsSteps(t) {
		assert.NotContains(t, s.Command, "^|", "step %d (%s)", s.Number, s.Description)
		assert.NotContains(t, s.Verify.Command, "^|", "verify of step %d (%s)", s.Number, s.Description)
	}
}

// TestWindowsUninstallUsesPlainPipes holds the uninstall plan to the
// same rule; it stops the same daemon the same way.
func TestWindowsUninstallUsesPlainPipes(t *testing.T) {
	plan := &install.Plan{}
	New(nil).buildWindowsUninstallSteps(plan, `C:\providers\ollama`)
	require.NotEmpty(t, plan.Steps)

	for _, s := range plan.Steps {
		assert.NotContains(t, s.Command, "^|", "step %d (%s)", s.Number, s.Description)
		assert.NotContains(t, s.Verify.Command, "^|", "verify of step %d (%s)", s.Number, s.Description)
	}
}
