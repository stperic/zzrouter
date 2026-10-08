package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStopCmd(t *testing.T) {
	cmd := NewStopCmd()

	require.NotNil(t, cmd, "NewStopCmd() should not return nil")
	assert.Equal(t, "stop <model_spec|id>", cmd.Use)
	assert.NotEmpty(t, cmd.Short, "cmd.Short should not be empty")
	assert.NotEmpty(t, cmd.Long, "cmd.Long should not be empty")
	require.NotNil(t, cmd.RunE, "cmd.RunE should not be nil")

	// Verify force flag exists
	forceFlag := cmd.Flags().Lookup("force")
	require.NotNil(t, forceFlag, "expected 'force' flag to exist")
	assert.Equal(t, "f", forceFlag.Shorthand)
}
