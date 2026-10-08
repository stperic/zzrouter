package detect

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckPortListening(t *testing.T) {
	// Port 1 should not be listening (requires root)
	assert.False(t, CheckPortListening(1))
}
