//go:build darwin

package service

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/require"
	"howett.net/plist"
)

func TestLaunchdInstalledPlistStopBudget(t *testing.T) {
	data, err := plist.Marshal(launchdDefinition("/opt/node", t.TempDir()), plist.XMLFormat)
	require.NoError(t, err)
	var decoded map[string]any
	_, err = plist.Unmarshal(data, &decoded)
	require.NoError(t, err)
	timeout, ok := decoded["ExitTimeOut"].(uint64)
	require.True(t, ok)
	require.Equal(t, constants.SupervisorStopTimeout, time.Duration(timeout)*time.Second)
}
