package servercli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// argsContain reports whether flag appears in args.
func argsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestDetachedNodeArgsPropagatesNoSetup pins the flag that decides
// whether an elevated child installs a system service. Dropping it
// across the detach boundary is silent: the node comes up and answers
// /health, but as a service account reading a different node.yaml.
func TestDetachedNodeArgsPropagatesNoSetup(t *testing.T) {
	cmd := NewStartCmd()
	require.NoError(t, cmd.Flags().Set("no-setup", "true"))

	args, err := detachedNodeArgs(cmd)
	require.NoError(t, err)
	assert.True(t, argsContain(args, "--no-setup"),
		"--no-setup must cross the detach boundary, got %v", args)
}

// TestDetachedNodeArgsOmitsNoSetupWhenUnset guards the other direction:
// the flag must not be synthesized for a caller that did not pass it,
// or an elevated `start --detach` would silently stop installing the
// service it is supposed to install.
func TestDetachedNodeArgsOmitsNoSetupWhenUnset(t *testing.T) {
	cmd := NewStartCmd()

	args, err := detachedNodeArgs(cmd)
	require.NoError(t, err)
	assert.False(t, argsContain(args, "--no-setup"),
		"--no-setup must not appear unless requested, got %v", args)
}

// TestDetachedNodeArgsCarriesPortAndDir covers the flags that were
// already propagated, so the extraction cannot quietly drop one.
func TestDetachedNodeArgsCarriesPortAndDir(t *testing.T) {
	cmd := NewStartCmd()
	require.NoError(t, cmd.Flags().Set("port", "9191"))
	require.NoError(t, cmd.Flags().Set("dir", "/tmp/zzcfg"))
	require.NoError(t, cmd.Flags().Set("debug", "true"))

	args, err := detachedNodeArgs(cmd)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(args), 1)
	assert.Equal(t, "start", args[0])
	assert.True(t, argsContain(args, "--port"), "args %v", args)
	assert.True(t, argsContain(args, "9191"), "args %v", args)
	assert.True(t, argsContain(args, "--dir"), "args %v", args)
	assert.True(t, argsContain(args, "/tmp/zzcfg"), "args %v", args)
	assert.True(t, argsContain(args, "--debug"), "args %v", args)
}

// TestDetachedNodeArgsDefaultsPort pins the port always being explicit:
// process detection matches on it.
func TestDetachedNodeArgsDefaultsPort(t *testing.T) {
	cmd := NewStartCmd()

	args, err := detachedNodeArgs(cmd)
	require.NoError(t, err)
	assert.True(t, argsContain(args, "--port"), "args %v", args)
}
