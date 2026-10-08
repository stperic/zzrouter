//go:build !windows

package install

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunStepPTY_SplitsOnCarriageReturn verifies the core difference
// between the PTY path and the non-PTY streaming path: a bar line that
// uses \r-overwrites (like pip) produces one discrete onLine call per
// tick instead of a single buffered string.
func TestRunStepPTY_SplitsOnCarriageReturn(t *testing.T) {
	var (
		mu    sync.Mutex
		lines []string
	)
	step := Step{
		Number: 1,
		// printf %b interprets backslash escapes so \r is a real CR.
		Command: `printf 'tick1\rtick2\rtick3\n'`,
		UsePTY:  true,
		StdoutLine: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
		},
	}
	err := executeStep(context.Background(), step)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	// Each \r-terminated segment should become its own onLine.
	assert.Contains(t, lines, "tick1")
	assert.Contains(t, lines, "tick2")
	assert.Contains(t, lines, "tick3")
}

// TestRunStepPTY_NonZeroExitErrorsWithTail confirms the error shape
// parity with the non-PTY streaming path.
func TestRunStepPTY_NonZeroExitErrorsWithTail(t *testing.T) {
	step := Step{
		Number:     1,
		Command:    `printf "pty-tail\n" && exit 1`,
		UsePTY:     true,
		StdoutLine: func(string) {},
	}
	err := executeStep(context.Background(), step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pty-tail")
}
