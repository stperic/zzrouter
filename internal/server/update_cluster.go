package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/version"
)

type updateApplyRequest struct {
	Version *string  `json:"version"`
	Nodes   []string `json:"nodes"`
}

type clusterUpdates struct{ server *Server }

func (b clusterUpdates) Status(ctx context.Context, node string) (update.UpdateStatus, error) {
	if node == b.server.node.Name() {
		if b.server.updateScheduler == nil {
			return update.UpdateStatus{}, fmt.Errorf("update scheduler is not initialized")
		}
		return b.server.updateScheduler.GetStatus(), nil
	}
	var result update.UpdateStatus
	err := b.query(ctx, node, "/update/status", http.MethodGet, nil, &result)
	return result, err
}
func (b clusterUpdates) Apply(ctx context.Context, node, target string) (string, error) {
	if node == b.server.node.Name() {
		return b.server.updateScheduler.ApplyVersionAsync(ctx, target)
	}
	var result struct {
		OperationID string `json:"operation_id"`
	}
	body, err := json.Marshal(struct {
		Version string `json:"version"`
	}{target})
	if err != nil {
		return "", err
	}
	err = b.query(ctx, node, "/update/apply", http.MethodPost, body, &result)
	return result.OperationID, err
}
func (b clusterUpdates) query(ctx context.Context, node, path, method string, body []byte, out any) error {
	client := b.server.getClusterClient()
	if ep := b.server.resolveUpdateEndpoint(node); ep != nil {
		node = ep.URL
	}
	return queryUpdateNode(ctx, client, node, path, method, body, out)
}

func queryUpdateNode(ctx context.Context, client mesh.ClusterClient, node, path, method string, body []byte, out any) error {
	if client == nil {
		return update.ErrNodeUnavailable
	}
	info, err := client.Unicast(ctx, node, "/zzrouter/v1/internal/version", &mesh.QueryParams{Method: http.MethodGet, Timeout: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("%w: %s", update.ErrNodeUnavailable, err)
	}
	if info.StatusCode != http.StatusOK {
		return fmt.Errorf("node %s version returned HTTP %d", node, info.StatusCode)
	}
	var peer version.VersionInfo
	if err := json.Unmarshal(info.Body, &peer); err != nil {
		return fmt.Errorf("read node version: %w", err)
	}
	if !slices.Contains(peer.Capabilities, version.CapabilityClusterUpdates) {
		return fmt.Errorf("node %s does not support cluster updates; upgrade it first", node)
	}
	response, err := client.Unicast(ctx, node, "/zzrouter/v1/internal"+path, &mesh.QueryParams{Method: method, Body: body, Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("%w: %s", update.ErrNodeUnavailable, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("node %s update returned HTTP %d: %.512s", node, response.StatusCode, response.Body)
	}
	if err := json.Unmarshal(response.Body, out); err != nil {
		return fmt.Errorf("decode node update: %w", err)
	}
	return nil
}

func (s *Server) selectUpdateNodes(requested []string) ([]string, error) {
	if requested == nil {
		return []string{s.node.Name()}, nil
	}
	if len(requested) == 0 || len(requested) > 64 {
		return nil, fmt.Errorf("select between 1 and 64 nodes")
	}
	if len(requested) == 1 && requested[0] == "all" {
		names := []string{s.node.Name()}
		for _, ep := range s.GetClusterEndpoints() {
			if ep == nil || ep.IsLocal {
				continue
			}
			name := s.updateNodeName(ep)
			if name == "" {
				return nil, fmt.Errorf("a configured peer has not reported a node name; wait for its first connection")
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if len(names) > 64 {
			return nil, fmt.Errorf("select at most 64 nodes")
		}
		return names, nil
	}
	nodes := make([]string, 0, len(requested))
	for _, name := range requested {
		if len(name) > 256 || strings.ContainsAny(name, "/:\\") || name == "all" || name == "" {
			return nil, fmt.Errorf("invalid node name %q", name)
		}
		canonical := s.node.Name()
		if !s.node.IsLocalNode(name) {
			ep := s.resolveUpdateEndpoint(name)
			if ep == nil {
				return nil, fmt.Errorf("unknown node %q", name)
			}
			canonical = s.updateNodeName(ep)
		}
		if slices.Contains(nodes, canonical) {
			return nil, fmt.Errorf("duplicate node %q", name)
		}
		nodes = append(nodes, canonical)
	}
	return nodes, nil
}

func (s *Server) updateNodeName(ep *mesh.Endpoint) string {
	if ep.NodeName != "" {
		return ep.NodeName
	}
	if s.nodeConfigStore != nil {
		if parsed, err := url.Parse(ep.URL); err == nil {
			if name := s.nodeConfigStore.Config().Cluster.Endpoints.NameFor(parsed.Host); name != "" {
				return name
			}
		}
	}
	return ep.Alias
}

func (s *Server) resolveUpdateEndpoint(name string) *mesh.Endpoint {
	if ep := s.resolveJobsEndpoint(name); ep != nil {
		return ep
	}
	for _, ep := range s.GetClusterEndpoints() {
		if ep != nil && !ep.IsLocal && strings.EqualFold(s.updateNodeName(ep), name) {
			return ep
		}
	}
	return nil
}

func (c *UpdateController) clusterRead(ctx *gin.Context, history bool) bool {
	if c.server == nil || !ctx.Request.URL.Query().Has("nodes") {
		return false
	}
	nodes, err := c.server.selectUpdateNodes(strings.Split(ctx.Query("nodes"), ","))
	if err != nil {
		BadRequest(ctx, err.Error())
		return true
	}
	results := make([]gin.H, 0, len(nodes))
	backend := clusterUpdates{c.server}
	for _, node := range nodes {
		item := gin.H{"node": node}
		if history {
			var result *update.UpdateHistory
			if node == c.server.node.Name() {
				result, err = c.scheduler().GetHistory()
			} else {
				result = &update.UpdateHistory{}
				err = backend.query(ctx.Request.Context(), node, "/update/history", http.MethodGet, nil, result)
			}
			item["history"] = result
		} else {
			var status update.UpdateStatus
			status, err = backend.Status(ctx.Request.Context(), node)
			item["status"] = status
		}
		if err != nil {
			delete(item, "status")
			delete(item, "history")
			item["error"] = err.Error()
			if errors.Is(err, update.ErrNodeUnavailable) {
				item["state"] = "pending"
			} else {
				item["state"] = "failed"
			}
		}
		results = append(results, item)
	}
	data := gin.H{"nodes": results}
	if c.server.updateRollouts != nil {
		data["rollouts"] = c.server.updateRollouts.History()
	}
	respondSuccess(ctx, "Node update state retrieved", data)
	return true
}

// HandleUpdateSettings reads or changes only scheduled updating on one node.
func (c *UpdateController) HandleUpdateSettings(ctx *gin.Context) {
	if c.requireScheduler(ctx) == nil {
		return
	}
	if c.server == nil {
		ServiceUnavailable(ctx, "node configuration is not initialized")
		return
	}
	requested := []string(nil)
	if name := ctx.Query("node"); name != "" {
		requested = []string{name}
	}
	nodes, err := c.server.selectUpdateNodes(requested)
	if err != nil || len(nodes) != 1 {
		BadRequest(ctx, "select one known node")
		return
	}
	var body []byte
	if ctx.Request.Method == http.MethodPatch {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if !BindJSONStrict(ctx, &req) {
			return
		}
		if req.Enabled == nil {
			BadRequest(ctx, "enabled is required and must be boolean")
			return
		}
		body, err = json.Marshal(req)
		if err != nil {
			InternalNodeError(ctx, err.Error())
			return
		}
	}
	if nodes[0] != c.server.node.Name() {
		var result struct {
			Data struct {
				Node    string `json:"node"`
				Enabled bool   `json:"enabled"`
			} `json:"data"`
		}
		err = (clusterUpdates{c.server}).query(ctx.Request.Context(), nodes[0], "/update/settings", ctx.Request.Method, body, &result)
		if err != nil {
			ServiceUnavailable(ctx, err.Error())
			return
		}
		respondSuccess(ctx, "Update scheduling settings retrieved", result.Data)
		return
	}
	c.localSettings(ctx, body)
}
func (c *UpdateController) localSettings(ctx *gin.Context, body []byte) {
	if ctx.Request.Method == http.MethodPatch {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			BadRequest(ctx, err.Error())
			return
		}
		if c.server.nodeConfigStore == nil {
			ServiceUnavailable(ctx, "node configuration is not initialized")
			return
		}
		if err := c.server.nodeConfigStore.SetUpdateEnabled(req.Enabled); err != nil {
			InternalNodeError(ctx, "persist update scheduling: "+err.Error())
			return
		}
		c.server.reconcileUpdateScheduling(nil)
	}
	respondSuccess(ctx, "Update scheduling settings retrieved", gin.H{"node": c.server.node.Name(), "enabled": c.scheduler().Enabled()})
}

func (s *Server) reconcileUpdateScheduling(_ *config.NodeConfig) config.ReloadDisposition {
	s.updateSchedulingMu.Lock()
	defer s.updateSchedulingMu.Unlock()
	s.updateScheduler.SetEnabled(s.nodeConfigStore.Config().Update.IsEnabled()) // architecture-exempt: update scheduling, not provider-service onboarding
	return config.DispositionApplied
}

// registerClusterUpdateRoutes is mounted only on the worker's mTLS engine.
func registerClusterUpdateRoutes(router *gin.RouterGroup, s *Server) {
	controller := NewUpdateController(func() *update.Scheduler { return s.updateScheduler })
	controller.server = s
	router.GET("/update/status", func(c *gin.Context) {
		if sched := controller.requireScheduler(c); sched != nil {
			c.JSON(http.StatusOK, sched.GetStatus())
		}
	})
	router.GET("/update/history", func(c *gin.Context) {
		if sched := controller.requireScheduler(c); sched != nil {
			result, err := sched.GetHistory()
			if err != nil {
				InternalNodeError(c, err.Error())
				return
			}
			c.JSON(http.StatusOK, result)
		}
	})
	router.POST("/update/apply", func(c *gin.Context) {
		var req struct {
			Version string `json:"version"`
		}
		if !BindJSONStrict(c, &req) {
			return
		}
		if err := update.ValidateVersion(req.Version); err != nil {
			BadRequest(c, err.Error())
			return
		}
		sched := controller.requireScheduler(c)
		if sched == nil {
			return
		}
		id, err := sched.ApplyVersionAsync(c.Request.Context(), req.Version)
		if err != nil {
			if errors.Is(err, update.ErrApplyInFlight) {
				RespondToError(c, newProblemError(http.StatusConflict, "Conflict", err.Error()))
			} else {
				InternalNodeError(c, err.Error())
			}
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"operation_id": id})
	})
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		router.Handle(method, "/update/settings", func(c *gin.Context) {
			controller.HandleUpdateSettings(c)
		})
	}
}

func (s *Server) cancelUpdateRollout(id string) error {
	if s.updateRollouts == nil {
		return nil
	}
	err := s.updateRollouts.Cancel(id)
	if errors.Is(err, jobs.ErrNotFound) {
		return nil
	}
	return err
}
