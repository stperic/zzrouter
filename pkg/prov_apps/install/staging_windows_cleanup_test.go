package install

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func windowsStaged() StagedDir {
	return StagedDir{
		Live:     `C:\Programs\Ollama`,
		Staging:  `C:\Programs\Ollama.incoming`,
		Previous: `C:\Programs\Ollama.previous`,
	}
}

// TestActivateWindows_TrailingCleanupIsBestEffort is the regression from
// a real install. Windows renames a directory holding a running .exe but
// refuses to delete one, so removing the displaced copy can fail long
// after the swap itself succeeded. Failing the step there reports an
// install that entirely worked as broken.
func TestActivateWindows_TrailingCleanupIsBestEffort(t *testing.T) {
	cmd := windowsStaged().ActivateWindowsCommand()

	last := strings.LastIndex(cmd, "Remove-Item")
	assert.Contains(t, cmd[last:], "-ErrorAction SilentlyContinue",
		"the cleanup after a completed swap must not fail the step")
}

// TestActivateWindows_FailuresAreTerminating is what makes the rollback
// reachable at all. PowerShell's file cmdlets fail non-terminatingly by
// default, so without this the catch never fires and a Move-Item that
// did not swap the tree falls through to the cleanup and exits 0,
// reporting a swap that never happened as a success.
func TestActivateWindows_FailuresAreTerminating(t *testing.T) {
	cmd := windowsStaged().ActivateWindowsCommand()

	assert.Contains(t, cmd, "$ErrorActionPreference='Stop'")
	assert.Less(t, strings.Index(cmd, "$ErrorActionPreference"), strings.Index(cmd, "Move-Item"),
		"the preference has to be set before anything it governs")
	assert.NotContains(t, cmd, "exit 0",
		"a blanket success exit would defeat the preference it sits after")
}

// TestPrepareWindows_FailuresAreTerminating holds the sibling command to
// the same rule, so the two cannot disagree about what a failure is.
func TestPrepareWindows_FailuresAreTerminating(t *testing.T) {
	cmd := PrepareWindowsCommand(`C:\Programs\Ollama.incoming`)

	assert.Contains(t, cmd, "$ErrorActionPreference='Stop'")
	assert.NotContains(t, cmd, "SilentlyContinue",
		"a staging directory that cannot be cleared is the accumulation staging exists to stop")
}

// TestActivateWindows_LeadingCleanupStaysFatal keeps the other end
// strict: if the previous slot cannot be cleared BEFORE the swap, the
// moves that follow would fail or land somewhere unintended, so that one
// is a real failure.
func TestActivateWindows_LeadingCleanupStaysFatal(t *testing.T) {
	cmd := windowsStaged().ActivateWindowsCommand()

	first := strings.Index(cmd, "Remove-Item")
	firstStmt := cmd[first : first+strings.Index(cmd[first:], ";")]
	assert.NotContains(t, firstStmt, "SilentlyContinue",
		"a previous slot that cannot be cleared has to stop the activation")
}

// TestActivateWindows_RollsBackOnFailedSwap pins the property the whole
// staging design exists for: a failure leaves a working install.
func TestActivateWindows_RollsBackOnFailedSwap(t *testing.T) {
	cmd := windowsStaged().ActivateWindowsCommand()

	assert.Contains(t, cmd, "catch", "the swap must be guarded")
	assert.Contains(t, cmd, "throw", "and a failed swap must still fail the step")
	swap := strings.Index(cmd, "try {")
	assert.Contains(t, cmd[swap:], `Move-Item 'C:\Programs\Ollama.previous' 'C:\Programs\Ollama'`,
		"rollback restores the displaced tree")
}
