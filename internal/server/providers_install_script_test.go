package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func TestInstallPlanForwardsPlannerExport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, node := range []string{"", "worker one"} {
		for _, mode := range []string{"command", "hidden hook", "old worker"} {
			t.Run(node+"/"+mode, func(t *testing.T) {
				plan := install.Plan{Provider: "engine", Platform: fsroot.Platform{OS: "linux"}, Steps: []install.Step{{Number: 1, Command: "printf 'ready'"}}}
				if mode == "hidden hook" {
					plan.Steps[0].PostExec = func(context.Context) error { return nil }
				}
				if mode != "old worker" {
					plan.PrepareScriptExport()
				}
				wire, err := json.Marshal(plan)
				require.NoError(t, err)
				router := &recordingRouter{mockRouter: mockRouter{response: &routing.Response{StatusCode: http.StatusOK, Body: wire}}}
				ctrl := &ProvidersController{providersService: NewProvidersService(router)}
				engine := gin.New()
				engine.POST("/zzrouter/v1/providers/:name/install/plan", ctrl.GetInstallPlan)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/zzrouter/v1/providers/engine/install/plan?node="+url.QueryEscape(node), strings.NewReader("{}")))
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				require.Len(t, router.routed, 1)
				require.Equal(t, localIfEmpty(node), router.routed[0].Node)
				require.Equal(t, "/zzrouter/v1/internal/providers/engine/install/plan", router.routed[0].Path)
				var envelope struct {
					Data client.InstallPlanResponse `json:"data"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
				if mode == "old worker" {
					require.Nil(t, envelope.Data.ScriptExport, "coordinator must not invent export permission")
					return
				}
				require.NotNil(t, envelope.Data.ScriptExport)
				require.Equal(t, plan.ScriptExport.Script, envelope.Data.ScriptExport.Script)
				require.Equal(t, plan.ScriptExport.Reason, envelope.Data.ScriptExport.Reason)
			})
		}
	}
}
