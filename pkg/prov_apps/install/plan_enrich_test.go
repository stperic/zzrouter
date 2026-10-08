package install

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnrichLoadFailure_AccessViolation guards the operator-facing hint
// path: when a step's binary fails to load with Windows ACCESS_VIOLATION
// the wrapped error MUST mention the cudart/runtime DLL diagnosis.
// Otherwise operators see "exit status 0xc0000005" with no actionable
// next step, which is exactly the bad UX that motivated this check.
func TestEnrichLoadFailure_AccessViolation(t *testing.T) {
	raw := errors.New("exit status 0xc0000005")
	wrapped := enrichLoadFailure(raw)
	require.Error(t, wrapped)
	assert.Contains(t, wrapped.Error(), "0xc0000005")
	assert.Contains(t, wrapped.Error(), "ACCESS_VIOLATION")
	assert.Contains(t, wrapped.Error(), "cudart")
	// Underlying err must still match — the hint wraps, doesn't replace.
	assert.ErrorIs(t, wrapped, raw)
}

func TestEnrichLoadFailure_DLLNotFound(t *testing.T) {
	raw := errors.New("exit status 0xc0000135: ")
	wrapped := enrichLoadFailure(raw)
	require.Error(t, wrapped)
	assert.Contains(t, wrapped.Error(), "DLL")
	assert.ErrorIs(t, wrapped, raw)
}

func TestEnrichLoadFailure_UnknownExitCodePassthrough(t *testing.T) {
	// A generic non-zero exit (e.g. 1) is NOT a load failure — the
	// process ran. Don't add a misleading DLL hint.
	raw := errors.New("exit status 1: out of memory")
	wrapped := enrichLoadFailure(raw)
	assert.Equal(t, raw, wrapped)
}

func TestEnrichLoadFailure_NilPassthrough(t *testing.T) {
	assert.Nil(t, enrichLoadFailure(nil))
}
