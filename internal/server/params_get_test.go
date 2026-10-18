package server

import (
	"encoding/json"
	"net/http"
	"testing"

	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stretchr/testify/require"
)

func TestParamsGetShippedDictionaryIsBounded(t *testing.T) {
	s, _ := assetTestNode(t)
	for _, tc := range []struct{ provider, grep, context string }{
		{"vllm", "", ""}, {"vllm", "context", "max-model-len"}, {"llamacpp", "context", "ctx-size"},
	} {
		t.Run(tc.provider+"/"+tc.grep, func(t *testing.T) {
			rest := assetRequest(t, s, http.MethodGet, apipath.ProviderSchema(tc.provider), nil)
			require.Equal(t, http.StatusOK, rest.Code, rest.Body.String())
			var full client.ProviderSchemaResponse
			require.NoError(t, json.Unmarshal(rest.Body.Bytes(), &full))
			require.Greater(t, len(full.Parameters), 100)
			result := callMCPWireTool(t, s, "zz_params_get", map[string]any{"provider": tc.provider, "grep": tc.grep})
			require.NotEqual(t, true, result["isError"], result)
			fields := result["structuredContent"].(map[string]any)
			accepts := fields["accepts"].(map[string]any)
			require.EqualValues(t, len(full.Parameters), fields["accepts_total"])
			require.Less(t, len(accepts), len(full.Parameters))
			if tc.context != "" {
				require.Contains(t, accepts, tc.context)
			}
			data, err := json.Marshal(fields)
			require.NoError(t, err)
			require.Less(t, len(data), 4096)
			entries, err := json.Marshal(accepts)
			require.NoError(t, err)
			t.Logf("%s grep=%q: reply=%d B, accepts=%d entries/%d B, total=%d", tc.provider, tc.grep, len(data), len(accepts), len(entries), len(full.Parameters))
		})
	}
}
