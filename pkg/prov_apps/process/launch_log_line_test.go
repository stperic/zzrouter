package process

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A file passed by content is logged by size whether or not it spans
// lines; flag values print as given.
func TestLaunchLogLine(t *testing.T) {
	oneLine := strings.Repeat("x", maxLoggedArgBytes+1)
	got := launchLogLine("mlx_lm.server", []string{"--chat-template", oneLine, "--model", "a b"})
	assert.Equal(t, `mlx_lm.server --chat-template <257 bytes> --model "a b"`, got)
	assert.Equal(t, "llama-server --ctx-size 8192", launchLogLine("llama-server", []string{"--ctx-size", "8192"}))
}
