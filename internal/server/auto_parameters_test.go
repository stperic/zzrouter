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

func TestAutoMemoryPatchValidationOnlyDeclaredFractionKey(t *testing.T) {
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
	assert.NotEmpty(t, validatePatch("vllm", &cfg, patch, sch, patchScope{}))
}

func TestAutoMemoryAmbiguousAliasesAreClientErrors(t *testing.T) {
	err := fmt.Errorf("%w: ambiguous memory parameter spellings", process.ErrMemoryBudgetInvalid)
	assert.Equal(t, http.StatusBadRequest, classifyLoadFailure(err).status)
	c, response := testGinContext("POST", "/zzrouter/v1/internal/runs")
	respondProviderErr(c, err)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}
