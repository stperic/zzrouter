package metadata

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadTargetClosedOwnersAndLegacyEncoding(t *testing.T) {
	require.Equal(t, OwnerCatalog, (DownloadTarget{}).EffectiveOwner())
	require.NoError(t, (DownloadTarget{}).Validate())
	require.NoError(t, (DownloadTarget{Owner: OwnerCatalog}).Validate())
	require.NoError(t, (DownloadTarget{Owner: OwnerProviderStore, Provider: "studio", Category: "vae", Name: "model"}).Validate())
	for _, target := range []DownloadTarget{{Owner: "unknown"}, {Provider: "studio"}, {Owner: OwnerProviderStore}, {Owner: OwnerCatalog, Name: "file"}} {
		require.Error(t, target.Validate())
	}
	b, err := json.Marshal(DownloadRequest{Repo: "org/model"})
	require.NoError(t, err)
	require.NotContains(t, string(b), "target")
	require.NotContains(t, string(b), "revision")
}
