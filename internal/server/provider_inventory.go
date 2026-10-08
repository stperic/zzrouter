package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// providerInventoryStatus runs only when publishing catalog evidence.
func (s *Server) providerInventoryStatus(ctx context.Context, failures map[string]string) []cache.InventoryUnavailable {
	reasons := maps.Clone(failures)
	if reasons == nil {
		reasons = make(map[string]string)
	}
	statuses := make(map[string]prov_apps.ProviderServiceStatus)
	if s.appsConfig != nil && s.providers.appMgr != nil {
		s.appsConfig.RangeApps(func(name string, cfg config.ServiceConfig) bool {
			if !cfg.IsEnabled() || !cfg.HasEndpoint() || cfg.IsCloudProvider() {
				return true
			}
			status, err := s.providers.appMgr.ProviderServiceStatus(ctx, name)
			if err != nil {
				if status.Managed {
					reasons[name] = utils.SanitizeErrorMessage(err.Error())
				}
				return true
			}
			statuses[name] = status
			if status.Managed && status.Supervisor != "external" && !status.Running {
				reasons[name] = "provider is not running"
			}
			return true
		})
	}
	var result []cache.InventoryUnavailable
	for provider, reason := range reasons {
		failure := cache.InventoryUnavailable{Node: s.node.Nodename(), Provider: provider, Reason: reason}
		if status, ok := statuses[provider]; ok {
			failure.Service = &status
		}
		result = append(result, failure)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Provider < result[j].Provider })
	return result
}

func (s *Server) providerInventoryFailure(node, provider string, missing bool) *httperr.Error {
	if s.model == nil || s.model.Cache == nil {
		return nil
	}
	for _, failure := range s.model.Cache.UnavailableInventories(node, provider) {
		stopped := failure.Service != nil && failure.Service.Managed && failure.Service.Supervisor != "external" && !failure.Service.Running
		if !missing && !stopped {
			continue
		}
		code := "model_inventory_unavailable"
		message := fmt.Sprintf("Model inventory for provider %q on node %q is unavailable: %s.", failure.Provider, failure.Node, failure.Reason)
		extra := map[string]any{"node": failure.Node, "provider": failure.Provider, "supervision": failure.Service}
		if stopped {
			code = "provider_not_running"
			message = fmt.Sprintf("Provider %q is not running on node %q (supervisor=%s, desired_running=%t).", failure.Provider, failure.Node, failure.Service.Supervisor, failure.Service.Desired)
			route := "/zzrouter/v1/providers/" + url.PathEscape(failure.Provider) + "/service/start?node=" + url.QueryEscape(failure.Node)
			extra["start_route"] = route
			message += " Recover with POST " + route + "."
		}
		if missing {
			message += " Model existence cannot be established while this inventory is unavailable."
		}
		return &httperr.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Code: code, Message: message, Extra: extra}
	}
	return nil
}

func (s *Server) admitProviderInventory(c *gin.Context, resolved *resolver.Resolved, nodeHint string) bool {
	node := resolved.Node
	if node == "" && resolved.Provider != "" {
		node = s.node.Nodename()
	}
	if resolved.Provider == "" && nodeHint != "" {
		node = nodeHint
		if s.node.IsLocalNode(nodeHint) {
			node = s.node.Nodename()
		}
	}
	if resolved.Provider == "" && s.node.IsLocalNode(node) && s.providers.appMgr != nil {
		if running, found := s.providers.appMgr.Instances().GetByModel(resolved.ModelName); found && running.GetStatus() == instance.StatusRunning {
			return true
		}
	}
	if problem := s.providerInventoryFailure(node, resolved.Provider, resolved.Provider == ""); problem != nil {
		recordErrorOnContext(c.Request.Context(), problem.Code, problem.Message)
		writeError(c.Writer, c.Request, *problem)
		return false
	}
	return true
}

func (s *Server) missingInventoryNode(req *http.Request) string {
	if s.node.IsWorker() {
		return s.node.Nodename()
	}
	hint := req.Header.Get("X-Node")
	if hint == "" {
		_, hint = s.splitModelRef(clientModelFromContext(req.Context()))
	}
	if hint != "" && s.node.IsLocalNode(hint) {
		return s.node.Nodename()
	}
	return hint
}
