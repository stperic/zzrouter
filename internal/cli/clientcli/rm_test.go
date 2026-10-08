package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRmCmd(t *testing.T) {
	cmd := NewRmCmd()

	require.NotNil(t, cmd, "NewRmCmd() should not return nil")
	assert.Equal(t, "rm [model]", cmd.Use)
	assert.NotEmpty(t, cmd.Short, "cmd.Short should not be empty")
	assert.NotEmpty(t, cmd.Long, "cmd.Long should not be empty")
	require.NotNil(t, cmd.RunE, "cmd.RunE should not be nil")

	// Verify expected flags exist
	flags := cmd.Flags()

	nodeFlag := flags.Lookup("node")
	require.NotNil(t, nodeFlag, "expected 'node' flag to exist")
	assert.Equal(t, "n", nodeFlag.Shorthand)
	assert.Equal(t, "*", nodeFlag.DefValue)

	modelFlag := flags.Lookup("model")
	require.NotNil(t, modelFlag, "expected 'model' flag to exist")
	assert.Equal(t, "m", modelFlag.Shorthand)

	forceFlag := flags.Lookup("force")
	require.NotNil(t, forceFlag, "expected 'force' flag to exist")
	assert.Equal(t, "f", forceFlag.Shorthand)
}
