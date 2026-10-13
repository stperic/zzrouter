package views

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestNodeRunRowsStayDistinctAndNamed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var data any = []any{}
		if req.URL.Path == "/zzrouter/v1/runs" {
			data = []pkgClient.Instance{
				{ID: "first", Node: "worker", App: "studio-a", Status: "running", BaseURL: "http://worker:8188"},
				{ID: "second", Node: "worker", App: "studio-b", Status: "running", BaseURL: "http://worker:8189"},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "success": true}))
	}))
	defer server.Close()
	client := pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL})
	rows, _, _, _, err := fetchCombinedModels(client, false, false)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "studio-a", rows[0].Model)
	require.Equal(t, "studio-b", rows[1].Model)
	require.Empty(t, rows[0].RawModel)
	require.Equal(t, "first", rows[0].instance.ID)
}
