package client

import (
	"encoding/json"
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
