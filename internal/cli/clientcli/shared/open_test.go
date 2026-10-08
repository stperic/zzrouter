package shared

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openCommand must name a launcher that exists on the host platform, and
// must pass the path as its own argument so spaces survive.
func TestOpenCommand_PerPlatform(t *testing.T) {
	file := filepath.Join(t.TempDir(), "my payload %#.json")
	name, args := openCommand(file)

	switch runtime.GOOS {
	case "darwin":
		assert.Equal(t, "open", name)
		assert.Equal(t, []string{file}, args)
	case "windows":
		assert.Equal(t, "rundll32", name)
		require.Len(t, args, 2)
		assert.Equal(t, "shell32.dll,ShellExec_RunDLL", args[0])
		assert.Equal(t, file, args[1])
	default:
		assert.Equal(t, "xdg-open", name)
		assert.Equal(t, []string{file}, args)
	}

	require.NotEmpty(t, name, "every supported platform needs a launcher")
	assert.Contains(t, args, file,
		"the path must stay one argument so spaces are not split")
}
