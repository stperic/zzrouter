package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyInstallCannotPromoteLegacyResult(t *testing.T) {
	for _, contract := range []string{"", "unknown_contract", "runtime_checks_v1", "install_steps_v1"} {
		t.Run(contract, func(t *testing.T) {
			body, err := json.Marshal(install.VerifyResult{Provider: "arbitrary", CheckContract: contract, AllOK: true})
			require.NoError(t, err)
			service := NewProvidersService(&mockRouter{response: &routing.Response{StatusCode: http.StatusOK, Body: body}})
			result, err := service.VerifyInstall(t.Context(), "arbitrary", "", "worker-1")
			require.NoError(t, err)
			supported := contract == install.RuntimeCheckContract || contract == "install_steps_v1"
			assert.Equal(t, supported, result.AllOK)
			if !supported {
				require.Len(t, result.Checks, 1)
				assert.Equal(t, "diagnostic_contract", result.Checks[0].Name)
				assert.Contains(t, result.Checks[0].Reason, "upgrade")
			}
		})
	}
}
