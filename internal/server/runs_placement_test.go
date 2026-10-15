package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerRunsPlacementWire(t *testing.T) {
	for _, layers := range []int64{0, 8, 29} {
		t.Run(map[int64]string{0: "CPU", 8: "partial", 29: "GPU"}[layers], func(t *testing.T) {
			run := instance.NewInstance("worker-run", "llamacpp", "model", 8080, 0, 0)
			run.SetLoaded(instance.Loaded{GPULayers: layers, Layers: 29, ContextLength: 4096})
			run.MarkRunning(false, nil)
			body, err := json.Marshal(map[string]any{"instances": []instance.InstanceInfo{run.ToInfo("worker")}})
			require.NoError(t, err)
			svc := NewRunsService(&mockRouter{response: &routing.Response{StatusCode: http.StatusOK, Body: body}})
			for _, node := range []string{"worker", "*"} {
				resp, err := svc.ListRuns(t.Context(), &ListRunsRequest{Node: node})
				require.NoError(t, err)
				require.Len(t, resp.Data, 1)
				assert.Equal(t, "worker", resp.Data[0].Node)
				assert.Equal(t, map[int64]string{0: "100% CPU", 8: "28% GPU / 72% CPU", 29: "100% GPU"}[layers], resp.Data[0].Processor)
				assert.Equal(t, 4096, resp.Data[0].ContextLength)
			}
		})
	}
}
