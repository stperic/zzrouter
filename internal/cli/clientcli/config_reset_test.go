package clientcli

import (
	"os"
	"testing"
	"time"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

func TestConfigResetWaitsForTransaction(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	cm := config.NewConfigManager("zzrouter")
	require.NoError(t, cm.SaveClientConfig(&config.ClientConfig{Node: config.NodeConnection{Address: "before-reset.invalid"}}))
	before, err := os.ReadFile(cm.GetClientConfigPath())
	require.NoError(t, err)
	owner, err := atomicfile.Lock(cm.GetClientConfigPath() + ".lock")
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		command := NewConfigCmd()
		command.SetArgs([]string{"reset", "--yes"})
		close(started)
		done <- command.Execute()
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("reset bypassed the active config transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	current, err := os.ReadFile(cm.GetClientConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, current)
	require.NoError(t, owner.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not finish after the transaction released")
	}
	current, err = os.ReadFile(cm.GetClientConfigPath())
	require.NoError(t, err)
	require.Equal(t, templates.GetClientTemplate(), string(current))
}
