package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

func TestAutoPatchValidationOnlyDeclaredKeys(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	store, err := pkgConfig.NewAppsConfigStore(root)
	require.NoError(t, err)
	sch := loadProviderSchema(store, "vllm")
	cfg, ok := store.Config().LookupApp("vllm")
	require.True(t, ok)
	patch := &paramPatchBody{Defaults: &defaultsPatch{Parameters: map[string]json.RawMessage{"gpu-memory-utilization": raw("auto")}}}
	require.Empty(t, validatePatch("vllm", &cfg, patch, sch, patchScope{}))
	patch.Defaults.Parameters = map[string]json.RawMessage{"max-model-len": raw("auto")}
	require.Empty(t, validatePatch("vllm", &cfg, patch, sch, patchScope{}))
	patch.Defaults.Parameters = map[string]json.RawMessage{"tensor-parallel-size": raw("auto")}
	assert.NotEmpty(t, validatePatch("vllm", &cfg, patch, sch, patchScope{}))
}

func TestAutoMemoryAmbiguousAliasesAreClientErrors(t *testing.T) {
	err := fmt.Errorf("%w: ambiguous memory parameter spellings", process.ErrMemoryBudgetInvalid)
	assert.Equal(t, http.StatusBadRequest, loadFailureWire(err).Status)
	c, response := testGinContext("POST", "/zzrouter/v1/internal/runs")
	RespondToError(c, err)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

func TestAutoPatchAPI(t *testing.T) {
	s, _ := assetTestNode(t)
	for _, tc := range []struct {
		provider, key string
		status        int
	}{
		{"vllm", "max-model-len", http.StatusOK},
		{"vllm", "gpu-memory-utilization", http.StatusOK},
		{"vllm", "dtype", http.StatusOK},
		{"llamacpp", "ctx-size", http.StatusOK},
		{"vllm", "tensor-parallel-size", http.StatusBadRequest},
	} {
		t.Run(tc.provider+"/"+tc.key, func(t *testing.T) {
			w := makeAuthRequest(t, s, http.MethodPatch, "/zzrouter/v1/providers/"+tc.provider+"/parameters", TestAdminKey, map[string]any{"defaults": map[string]any{"parameters": map[string]any{tc.key: "auto"}}})
			require.Equal(t, tc.status, w.Code, string(w.Body))
			if tc.status == http.StatusOK {
				cfg, ok := s.configStore.Config().LookupApp(tc.provider)
				require.True(t, ok)
				assert.Equal(t, "auto", cfg.Defaults.Parameters[tc.key])
			}
		})
	}
}
