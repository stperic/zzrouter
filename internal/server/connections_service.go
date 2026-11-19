package server

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

type connectionEvidence struct {
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
}

type connectionInfo struct {
	Provider             string                 `json:"provider"`
	Node                 string                 `json:"node"`
	RunID                string                 `json:"run_id,omitempty"`
	Model                string                 `json:"model,omitempty"`
	Runtime              string                 `json:"runtime,omitempty"`
	TargetRevision       string                 `json:"target_revision,omitempty"`
	ConnectionRevision   string                 `json:"connection_revision,omitempty"`
	State                string                 `json:"state"`
	NativeURL            string                 `json:"native_url,omitempty"`
	RoutedURLs           []string               `json:"routed_urls"`
	Routes               []string               `json:"routes,omitempty"`
	BrowserProxyURL      string                 `json:"browser_proxy_url,omitempty"`
	BrowserAvailable     bool                   `json:"browser_available,omitempty"`
	Listener             *instance.ListenerInfo `json:"listener"`
	NativeAuthentication string                 `json:"native_authentication"`
	RoutedAuthentication string                 `json:"routed_authentication"`
	RemoteReachability   string                 `json:"remote_reachability"`
	Evidence             connectionEvidence     `json:"evidence"`
}

func (connection connectionInfo) target() prov_apps.OperationTarget {
	return prov_apps.OperationTarget{Provider: connection.Provider, Node: connection.Node, RunID: connection.RunID, Model: connection.Model, Runtime: connection.Runtime}
}

func (connection connectionInfo) matches(c *gin.Context) bool {
	for key, value := range map[string]string{"provider": connection.Provider, "node": connection.Node, "run_id": connection.RunID, "model": connection.Model, "runtime": connection.Runtime} {
		if wanted := c.Query(key); wanted != "" && wanted != "*" && value != wanted {
			return false
		}
	}
	return true
}

// safeConnectionURL publishes no credentials, query values, fragments or opaque URLs.
func safeConnectionURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" {
		return ""
	}
	parsed.User, parsed.RawQuery, parsed.Fragment, parsed.RawFragment = nil, "", "", ""
	parsed.ForceQuery = false
	return parsed.String()
}

func connectionAuthentication(upstream backend.Upstream) string {
	if upstream.ForwardsCallerKey() {
		return "caller_key"
	}
	if upstream.API() != nil {
		return "provider_credential"
	}
	return "none"
}

func (s *Server) localConnectionInfo(c *gin.Context) []connectionInfo {
	result := []connectionInfo{}
	evidence := connectionEvidence{Source: "run_snapshot", ObservedAt: utils.NowUTC().Format("2006-01-02T15:04:05Z07:00")}
	if s.providers.appMgr != nil {
		for _, run := range s.providers.appMgr.ListInstances(s.node.Name()) {
			result = append(result, s.runConnectionInfo(c, run, evidence))
		}
	}
	// External daemon endpoints belong to their node even when no model is loaded.
	// Hosted/managed connections remain coordinator-owned.
	if s.backend != nil {
		s.appsConfig().RangeApps(func(name string, svc config.ServiceConfig) bool {
			if !svc.IsEnabled() || prov_apps.Lifecycle(svc) == prov_apps.LifecycleProcess || s.node.IsWorker() && svc.Mode != constants.AppModeExternal {
				return true
			}
			target, ok := s.backend.Target(name)
			if !ok {
				return true
			}
			auth := connectionAuthentication(target.Upstream)
			if svc.Runtime.API.SendsOwnCredential() {
				auth = "provider_credential"
			}
			state := "unconfigured"
			configured := target.Configured || !svc.Runtime.API.SendsOwnCredential()
			if svc.Connection != nil {
				status, err := s.backend.Connection(name)
				configured = configured && err == nil && status.Mode != secrets.Disabled
			}
			if configured {
				state = "configured"
			}
			result = append(result, connectionInfo{Provider: name, Node: s.node.Name(), ConnectionRevision: target.Revision, State: state, NativeURL: safeConnectionURL(target.Endpoint), RoutedURLs: []string{}, NativeAuthentication: auth, RoutedAuthentication: "zzrouter", RemoteReachability: "unknown", Evidence: connectionEvidence{Source: "provider_connection", ObservedAt: evidence.ObservedAt}, Listener: &instance.ListenerInfo{Scope: "unknown", Authentication: auth, Source: "provider_connection"}})
			return true
		})
	}
	return result
}

func (s *Server) runConnectionInfo(c *gin.Context, run instance.InstanceInfo, evidence connectionEvidence) connectionInfo {
	view := connectionInfo{Provider: run.Provider, Node: run.Node, RunID: run.ID, Model: run.Model, Runtime: run.Runtime, TargetRevision: run.TargetRevision, State: string(run.Status), RoutedURLs: []string{}, NativeAuthentication: "unknown", RoutedAuthentication: "zzrouter", RemoteReachability: "unknown", Evidence: evidence, Listener: &instance.ListenerInfo{Scope: "unknown", Authentication: "unknown", Source: "run_snapshot"}}
	if run.Status == instance.StatusRunning && !run.RecordOnly && run.Listener != nil {
		listener := *run.Listener
		listener.URL = safeConnectionURL(listener.URL)
		view.Listener, view.NativeURL, view.NativeAuthentication = &listener, listener.URL, listener.Authentication
	}
	if run.Status == instance.StatusRunning && !run.RecordOnly && s.backend != nil {
		if svc, found := s.appsConfig().LookupApp(run.Provider); found && svc.Mode == constants.AppModeExternal {
			if target, ok := s.backend.Target(run.Provider); ok {
				view.NativeURL, view.ConnectionRevision = safeConnectionURL(target.Endpoint), target.Revision
				view.NativeAuthentication = connectionAuthentication(target.Upstream)
				view.Listener.Authentication, view.Listener.Source = view.NativeAuthentication, "provider_connection"
			}
		}
	}
	if run.Status == instance.StatusRunning && !run.RecordOnly {
		endpoint, err := s.providers.appMgr.RunUpstreamURL(c.Request.Context(), run.ID)
		if err != nil {
			view.NativeURL, view.Listener.URL = "", ""
		} else if view.NativeURL == "" {
			view.NativeURL = safeConnectionURL(endpoint)
			view.Listener.URL, view.Listener.Source = view.NativeURL, "run_upstream"
			view.Evidence.Source = "run_snapshot_and_upstream"
		}
	}
	if _, _, err := s.providers.appMgr.BrowserRun(c.Request.Context(), run.ID); err == nil {
		view.BrowserAvailable = true
	}
	return view
}

func (s *Server) handleInternalListConnections(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": s.localConnectionInfo(c)})
}

func (s *Server) readConnections(c *gin.Context) ([]connectionInfo, error) {
	node := c.Query("node")
	if node == "" {
		node = "*"
	}
	if node != "*" && !s.knownClusterNodes()[node] {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", "node must be an exact known node name")
	}
	response, err := routeAndParse[struct {
		Data []connectionInfo `json:"data"`
	}](c.Request.Context(), s.cluster.router, http.MethodGet, "/zzrouter/v1/internal/connections", node, nil)
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (s *Server) connectionVisible(c *gin.Context, connection connectionInfo) (bool, []string) {
	allowed := s.catalogAllowed(c, connection.Model, connection.Provider)
	routes := []string{}
	if s.model.Groups != nil {
		for name, group := range s.model.Groups.List() {
			for _, replica := range group.Replicas {
				if replica.App != connection.Provider || connection.Model != "" && replica.Model != connection.Model || replica.Node != "" && replica.Node != connection.Node {
					continue
				}
				if s.catalogAllowed(c, replica.Model, replica.App, name) {
					allowed = true
					routes = append(routes, name)
					break
				}
			}
		}
	}
	if !allowed && connection.Model == "" && s.model.Cache != nil {
		for _, model := range s.model.Cache.GetAllModels() {
			if model.Provider == connection.Provider && s.catalogAllowed(c, model.Name, model.Provider) {
				allowed = true
				break
			}
		}
	}
	sort.Strings(routes)
	return allowed, routes
}

func (s *Server) handleListConnections(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	offset, limit, ok := providerDiscoveryPage(c)
	if !ok {
		return
	}
	connections, err := s.readConnections(c)
	if err != nil {
		RespondToError(c, err)
		return
	}
	views := []connectionInfo{}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	// Only the in-process MCP marker can supply a scheme captured at its actual ingress.
	if mcpserver.IsDispatch(c.Request.Context()) && c.Request.URL.Scheme == "https" {
		scheme = "https"
	}
	base := safeConnectionURL((&url.URL{Scheme: scheme, Host: c.Request.Host}).String())
	for _, view := range connections {
		if !view.matches(c) {
			continue
		}
		allowed, routes := s.connectionVisible(c, view)
		if !allowed {
			continue
		}
		view.Routes = routes
		view.NativeURL = safeConnectionURL(view.NativeURL)
		if view.Listener != nil {
			view.Listener.URL = safeConnectionURL(view.Listener.URL)
		}
		view.BrowserProxyURL = ""
		if base != "" {
			if svc, ok := s.appsConfig().LookupApp(view.Provider); ok && svc.Capabilities != nil && len(svc.Capabilities.EffectiveWireEndpoints()) > 0 {
				view.RoutedURLs = []string{strings.TrimRight(base, "/") + "/v1"}
			}
		}
		if view.State == string(instance.StatusRunning) && view.BrowserAvailable {
			view.BrowserProxyURL = s.providerBrowserURL(view.Node, view.RunID)
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		a, b := views[i], views[j]
		return a.Provider+"\x00"+a.Node+"\x00"+a.RunID < b.Provider+"\x00"+b.Node+"\x00"+b.RunID
	})
	total := len(views)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	respondList(c, views[offset:end], total, end < total)
}
