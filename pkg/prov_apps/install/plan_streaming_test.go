package install

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteStep_StdoutLineForwardsEveryLine pins the streaming
// contract: when Step.StdoutLine is non-nil, executeStep pipes the
// command's merged stdout+stderr line-by-line to the hook. Lines are
// seen in order; exit status zero propagates as a nil error.
func TestExecuteStep_StdoutLineForwardsEveryLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX printf not assumed on Windows")
	}
	var (
		mu    sync.Mutex
		lines []string
	)
	step := Step{
		Number:  1,
		Command: `printf "alpha\nbeta\ngamma\n"`,
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
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, lines)
}

// TestExecuteStep_StdoutLine_NonZeroExitErrorsWithTail confirms that a
// failing command surfaces its last-N lines in the returned error so
// operators can see what pip said before it gave up.
func TestExecuteStep_StdoutLine_NonZeroExitErrorsWithTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	step := Step{
		Number:     1,
		Command:    `printf "line1\nline2\nline3\n" && exit 2`,
		StdoutLine: func(string) {},
	}
	err := executeStep(context.Background(), step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line3")
}

// TestExecuteStep_StdoutLine_MergesStderr verifies that stderr is
// folded into the same stream as stdout. pip writes "Downloading" to
// stderr under some configurations, so losing stderr would lose the
// exact lines the parser needs.
func TestExecuteStep_StdoutLine_MergesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	var (
		mu    sync.Mutex
		lines []string
	)
	step := Step{
		Number:  1,
		Command: `printf "out1\n"; printf "err1\n" 1>&2; printf "out2\n"`,
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
	joined := strings.Join(lines, "|")
	assert.Contains(t, joined, "out1")
	assert.Contains(t, joined, "err1")
	assert.Contains(t, joined, "out2")
}
