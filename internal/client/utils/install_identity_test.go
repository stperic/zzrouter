package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallClientCarriesAcceptedPlanIdentity(t *testing.T) {
	var bodies []map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"data":{"accepted":true,"job_id":"job","passed":true}}`))
		require.NoError(t, err)
	}))
	plan := &InstallPlanResponse{Provider: "mlx-vlm", Version: "1.2.3", Action: "install", PlanID: "immutable"}
	_, err := c.ExecuteInstallStep("mlx", "mac", 2, plan)
	require.NoError(t, err)
	_, err = c.VerifyInstallStep("mlx", "mac", 2, plan)
	require.NoError(t, err)
	_, err = c.InstallProvider("mlx", "mac", plan)
	require.NoError(t, err)
	require.Len(t, bodies, 3)
	for _, body := range bodies {
		assert.Equal(t, "immutable", body["expected_plan_id"])
		assert.Equal(t, "mlx-vlm", body["runtime"])
		assert.Equal(t, "1.2.3", body["version"])
		assert.Equal(t, "install", body["action"])
	}
}

func TestInstallClientPreservesComponentSelectionIncludingExplicitEmpty(t *testing.T) {
	for _, selected := range [][]string{{"manager"}, {}} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			var bodies []map[string]any
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				bodies = append(bodies, body)
				_, err := w.Write([]byte(`{"data":{"accepted":true,"job_id":"job","passed":true}}`))
				require.NoError(t, err)
			}))
			plan := &InstallPlanResponse{Provider: "comfyui", PlanID: "components", Force: true, ResolvedInstall: &InstallRecipeResponse{SelectedComponents: selected}}
			_, err := c.InstallProvider("comfyui", "worker", plan)
			require.NoError(t, err)
			_, err = c.ExecuteInstallStep("comfyui", "worker", 2, plan)
			require.NoError(t, err)
			_, err = c.VerifyInstallStep("comfyui", "worker", 2, plan)
			require.NoError(t, err)
			for _, body := range bodies {
				require.Equal(t, true, body["force"])
				require.Equal(t, "components", body["expected_plan_id"])
				values, ok := body["components"].([]any)
				require.True(t, ok, "explicit empty must remain an array")
				require.Len(t, values, len(selected))
				if len(values) > 0 {
					require.Equal(t, "manager", values[0])
				}
			}
		})
	}
}
