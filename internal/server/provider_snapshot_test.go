package server

import (
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/require"
)

func TestNodeProviderSnapshotDoesNotInventRunningState(t *testing.T) {
	info := prov_apps.LocalProviderInfo{Key: "generic", Name: "generic", State: prov_apps.StateLive}
	_, present := nodeProviderInfo(info)["running"]
	require.False(t, present, "enabled on-demand and unknown external readiness are not observed processes")
	info.Service = &prov_apps.ProviderServiceStatus{Provider: "generic", Supervisor: "zzRouter", Managed: true, Running: false}
	result := nodeProviderInfo(info)
	require.Equal(t, false, result["running"])
	require.Equal(t, "zzRouter", result["service"].(prov_apps.ProviderServiceStatus).Supervisor)
	info.Service.Running = true
	require.Equal(t, true, nodeProviderInfo(info)["running"])
}

func TestProviderSnapshotIsReadableByProtocolFiveDecoder(t *testing.T) {
	info := prov_apps.LocalProviderInfo{Key: "generic", Name: "generic", State: prov_apps.StateLive,
		Service: &prov_apps.ProviderServiceStatus{Provider: "generic", Supervisor: "zzRouter", Managed: true}}
	data, err := json.Marshal(info)
	require.NoError(t, err)
	// The shipped health decoder uses ordinary JSON decoding, without strict config decoding.
	var old struct {
		Key, Name          string
		Enabled, Installed bool
	}
	require.NoError(t, json.Unmarshal(data, &old))
	require.Equal(t, "generic", old.Key)
	require.True(t, old.Enabled)
	require.True(t, old.Installed)
}
