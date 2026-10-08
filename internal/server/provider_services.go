package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host/service"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

type providerServices interface {
	ProviderServiceStatus(context.Context, string) (prov_apps.ProviderServiceStatus, error)
	ControlProviderService(context.Context, string, string) (prov_apps.ProviderServiceStatus, error)
	ApplyProviderService(context.Context, string) (*service.ApplyResult, error)
	ProviderManagedBinary(string) string
}

func (e *ParamsExecutor) serviceEnv(cfg *pkgConfig.ServiceConfig) map[string]string {
	return pkgConfig.FlattenEnvironment(cfg.Resolve(e.nodename(), "").Environment)
}

// HandleInternalGetServiceStatus observes ownership and endpoint readiness on this node.
func (e *ParamsExecutor) HandleInternalGetServiceStatus(c *gin.Context) {
	cfg, err := getAppConfig(e.appsConfig(), c.Param("name"))
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	if e.services == nil {
		ServiceUnavailable(c, "provider lifecycle manager is unavailable")
		return
	}
	status, err := e.services.ProviderServiceStatus(c.Request.Context(), c.Param("name"))
	if err != nil {
		e.serviceError(c, err)
		return
	}
	e.writeServiceStatus(c, cfg, status, e.checkHealth(c.Request.Context(), cfg))
}

func (e *ParamsExecutor) writeServiceStatus(c *gin.Context, cfg *pkgConfig.ServiceConfig, status prov_apps.ProviderServiceStatus, healthy bool) {
	if status.Supervisor == "external" {
		status.Running = healthy
	}
	var manager *string
	if status.Supervisor != "external" {
		manager = &status.Supervisor
	}
	result := struct {
		prov_apps.ProviderServiceStatus
		Node          string           `json:"node"`
		Mode          string           `json:"mode"`
		Manager       *string          `json:"manager"`
		Healthy       bool             `json:"healthy"`
		EnvState      process.EnvState `json:"env_state,omitempty"`
		RunningModels []string         `json:"running_models,omitempty"`
	}{ProviderServiceStatus: status, Node: e.nodename(), Mode: cfg.Mode, Manager: manager, Healthy: healthy}
	if healthy {
		result.RunningModels = e.listRunningModelNames(c.Request.Context(), status.Provider)
	}
	if binary := e.services.ProviderManagedBinary(status.Provider); binary != "" {
		result.EnvState = process.BinaryEnvState(binary, e.serviceEnv(cfg))
	}
	c.JSON(http.StatusOK, result)
}

// HandleServiceStatus routes observations to the requested node over mTLS.
func (e *ParamsExecutor) HandleServiceStatus(c *gin.Context) {
	e.relayToNode(c, http.MethodGet, "service/status", nil)
}

// HandleServiceControl binds a public route to one fixed lifecycle action.
func (e *ParamsExecutor) HandleServiceControl(action string) gin.HandlerFunc {
	return func(c *gin.Context) { e.relayToNode(c, http.MethodPost, "service/"+action, nil) }
}

// HandleInternalServiceControl executes a fixed action on this node only.
func (e *ParamsExecutor) HandleInternalServiceControl(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg, err := getAppConfig(e.appsConfig(), c.Param("name"))
		if err != nil {
			NotFound(c, err.Error())
			return
		}
		if e.services == nil {
			ServiceUnavailable(c, "provider lifecycle manager is unavailable")
			return
		}
		status, err := e.services.ControlProviderService(c.Request.Context(), c.Param("name"), action)
		if err != nil {
			e.serviceError(c, err)
			return
		}
		healthy := false
		if action != "stop" {
			healthy = e.waitForHealth(c.Request.Context(), cfg, 30*time.Second)
			status, err = e.services.ProviderServiceStatus(c.Request.Context(), c.Param("name"))
			if err != nil {
				e.serviceError(c, err)
				return
			}
		}
		e.writeServiceStatus(c, cfg, status, healthy)
	}
}

// HandleApplyService applies a provider's configured environment on its owning node.
func (e *ParamsExecutor) HandleApplyService(c *gin.Context) {
	e.relayToNode(c, http.MethodPost, "service/apply", nil)
}

// HandleInternalApplyService delegates to the same managed ownership gate as controls.
func (e *ParamsExecutor) HandleInternalApplyService(c *gin.Context) {
	cfg, err := getAppConfig(e.appsConfig(), c.Param("name"))
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	if e.services == nil {
		ServiceUnavailable(c, "provider lifecycle manager is unavailable")
		return
	}
	runningModels := e.listRunningModelNames(c.Request.Context(), c.Param("name"))
	result, err := e.services.ApplyProviderService(c.Request.Context(), c.Param("name"))
	if err != nil {
		e.serviceError(c, err)
		return
	}
	healthy := result.Restarted && e.waitForHealth(c.Request.Context(), cfg, 30*time.Second)
	if err := c.Request.Context().Err(); err != nil {
		e.serviceError(c, err)
		return
	}
	if result.Restarted && len(runningModels) > 0 && result.Warning == "" {
		result.Warning = "Restart interrupted loaded models; they reload on the next request."
	}
	c.JSON(http.StatusOK, gin.H{"provider": c.Param("name"), "node": e.nodename(), "manager": result.Manager,
		"applied": result.Applied, "restarted": result.Restarted, "healthy": healthy,
		"env_vars_applied": result.EnvVarsCount, "error": result.Error,
		"running_models_before_restart": runningModels,
		"manual_instructions":           result.Instructions, "warning": result.Warning})
}

func (e *ParamsExecutor) serviceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, prov_apps.ErrExternalService):
		Forbidden(c, err.Error())
	case errors.Is(err, prov_apps.ErrServiceDisabled):
		Conflict(c, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		RequestTimeout(c, err.Error())
	default:
		ServiceUnavailable(c, err.Error())
	}
}

func (e *ParamsExecutor) relayToNode(c *gin.Context, method, suffix string, body []byte) {
	path := fmt.Sprintf("/zzrouter/v1/internal/providers/%s/%s", url.PathEscape(c.Param("name")), suffix)
	routeAndRelay(c, e.router, method, path, localIfEmpty(QueryNode(c)), body)
}
