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

func TestProviderSchemaProjectsObservationAndCalibration(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{
		TestNodeConfig: DefaultTestNodeConfig(),
		providers:      []string{"comfyui", "vllm", "mlx"},
	})
	for _, tc := range []struct {
		provider, observe, calibration string
	}{
		{"comfyui", `{"path":"/queue","format":"json","busy":["queue_running","queue_pending"]}`, ""},
		{"vllm", `{"path":"/metrics","format":"prometheus","running":"vllm:num_requests_running","waiting":"vllm:num_requests_waiting","kv_cache_usage":"vllm:kv_cache_usage_perc"}`, `{"output":{"ignore_eos":true}}`},
		{"mlx", `{"path":"/metrics","format":"json","running":"summary.in_flight","waiting":"server.request_queue_depth","runtimes":["mlx-vlm"]}`, ""},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			resp := assetRequest(t, s, http.MethodGet, apipath.ProviderSchema(tc.provider), nil)
			require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
			var wire map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &wire))
			require.JSONEq(t, tc.observe, string(wire["observe"]))
			if tc.calibration == "" {
				require.NotContains(t, wire, "calibration")
			} else {
				require.JSONEq(t, tc.calibration, string(wire["calibration"]))
			}
		})
	}
}
