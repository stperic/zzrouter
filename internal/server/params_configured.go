package server

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// configuredParameters is an editing projection, never a resolved launch view.
// Values remain strings; flag names use the same identity as PATCH.
type configuredParameters struct {
	Provider           string            `json:"provider"`
	Scope              string            `json:"scope"`
	Node               string            `json:"node,omitempty"`
	Model              string            `json:"model,omitempty"`
	Endpoint           string            `json:"endpoint,omitempty"`
	Parameters         map[string]string `json:"parameters"`
	Environment        map[string]string `json:"environment"`
	AvailableEndpoints []string          `json:"available_endpoints"`
}

func (e *ParamsExecutor) handleConfiguredParameters(c *gin.Context) {
	// A worker's cached provider tree is not authoritative editing evidence.
	if e.rejectOnWorker(c) {
		return
	}
	provider := c.Param("name")
	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	out, err := configuredParameterScope(cfg, c.Query("scope"), c.Query("node"), c.Query("model"), c.Query("endpoint"))
	if err != nil {
		var conflicts paramErrors
		if errors.As(err, &conflicts) {
			RespondWithParamErrors(c, http.StatusConflict, "Conflicting parameter spellings at this scope", conflicts)
			return
		}
		BadRequest(c, err.Error())
		return
	}
	out.Provider = provider
	c.JSON(http.StatusOK, out)
}

func configuredParameterScope(cfg *pkgConfig.ServiceConfig, scope, node, model, endpoint string) (configuredParameters, error) {
	out := configuredParameters{Scope: scope, Node: node, Model: model, Endpoint: endpoint}
	var endpoints map[string]pkgConfig.EndpointOverlay
	switch scope {
	case "defaults":
		if node != "" || model != "" {
			return out, fmt.Errorf("defaults scope does not accept node or model")
		}
		if cfg.Defaults != nil {
			out.Parameters, out.Environment, endpoints = cfg.Defaults.Parameters, cfg.Defaults.Environment, cfg.Defaults.Endpoints
		}
	case "model":
		if model == "" || node != "" {
			return out, fmt.Errorf("model scope requires model and does not accept node")
		}
		cell := cfg.Models[model]
		out.Parameters, out.Environment, endpoints = cell.Parameters, cell.Environment, cell.Endpoints
	case "node":
		if node == "" || model != "" {
			return out, fmt.Errorf("node scope requires node and does not accept model")
		}
		cell := cfg.Nodes[node]
		out.Parameters, out.Environment, endpoints = cell.Parameters, cell.Environment, cell.Endpoints
	case "node-model":
		if node == "" || model == "" {
			return out, fmt.Errorf("node-model scope requires node and model")
		}
		cell := cfg.Nodes[node].Models[model]
		out.Parameters, out.Environment, endpoints = cell.Parameters, cell.Environment, cell.Endpoints
	default:
		return out, fmt.Errorf("scope must be defaults, model, node or node-model")
	}
	if endpoint != "" {
		if err := cfg.ValidateRunEndpoint(endpoint); err != nil {
			return out, err
		}
		out.Parameters, out.Environment = endpoints[endpoint].Parameters, endpoints[endpoint].Environment
	}
	// PATCH addresses a flag, not its stored spelling. Project just this map
	// with the same owner, refusing ambiguous aliases instead of hiding one.
	raw := out.Parameters
	flags := map[string]pkgConfig.ResolvedValue{}
	pkgConfig.LayParameters(flags, raw, pkgConfig.ResolvedValue{})
	out.Parameters = make(map[string]string, len(flags))
	var conflicts paramErrors
	for _, flag := range slices.Sorted(maps.Keys(flags)) {
		entry := flags[flag]
		out.Parameters[flag] = raw[entry.Key]
		if len(entry.Conflicts) > 0 {
			conflicts = append(conflicts, schema.Duplicate(append([]string{entry.Key}, entry.Conflicts...))...)
		}
	}
	if len(conflicts) > 0 {
		return out, conflicts
	}
	if out.Environment == nil {
		out.Environment = map[string]string{}
	}
	out.AvailableEndpoints = cfg.RunEndpoints()
	return out, nil
}
