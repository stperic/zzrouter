package fallback

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutcomeConstants(t *testing.T) {
	// Verify outcome constants are distinct and non-empty
	outcomes := []string{
		OutcomeSuccess,
		OutcomeRetriable,
		OutcomeNonRetriable,
		OutcomeCooldownSkip,
		OutcomeOnDemandSkip,
	}

	seen := make(map[string]bool)
	for _, o := range outcomes {
		assert.NotEmpty(t, o, "outcome constant should not be empty")
		assert.False(t, seen[o], "outcome %q should be unique", o)
		seen[o] = true
	}
}
