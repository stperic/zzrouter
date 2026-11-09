package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
)

// testSignalConfig keeps existing domain fixtures' coordinator HTTP adapters.
// The test coordinator consumes typed operations; the worker has no HTTP client.
type testSignalConfig struct {
	IsWorker        func() bool
	MTLSClient      func() *http.Client
	CoordinatorURL  func() string
	WorkerPublicURL func() string
}

func newTestSignals(t *testing.T, cfg testSignalConfig) *clustersignal.Signaler {
	t.Helper()
	s := clustersignal.New(clustersignal.Config{IsWorker: cfg.IsWorker})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]bool)
	wg.Add(1)
	go func() {
		defer wg.Done()
		incarnation := ""
		for {
			mu.Lock()
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			mu.Unlock()
			batch, err := s.Wait(ctx, clustersignal.Poll{Seen: ids, Incarnation: incarnation})
			if err != nil {
				return
			}
			incarnation = batch.Incarnation
			for _, op := range batch.Operations {
				mu.Lock()
				if seen[op.ID] {
					mu.Unlock()
					continue
				}
				seen[op.ID] = true
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					path := "/zzrouter/v1/internal/models/refresh"
					method := http.MethodPost
					body := []byte("{}")
					switch op.Kind {
					case clustersignal.ProviderEnabled:
						path = "/zzrouter/v1/internal/providers/" + url.PathEscape(op.Provider) + "/enabled"
						method = http.MethodPut
						body, _ = json.Marshal(map[string]bool{"enabled": op.Enabled})
					case clustersignal.Goodbye:
						path = "/zzrouter/v1/internal/goodbye"
						body, _ = json.Marshal(map[string]string{"node_url": cfg.WorkerPublicURL()})
					}
					req, err := http.NewRequestWithContext(ctx, method, cfg.CoordinatorURL()+path, bytes.NewReader(body))
					result := clustersignal.Result{ID: op.ID}
					if err == nil {
						req.Header.Set("Content-Type", "application/json")
						var resp *http.Response
						resp, err = cfg.MTLSClient().Do(req)
						if resp != nil {
							data, _ := io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							if resp.StatusCode != 200 {
								var problem struct {
									Code   string `json:"code"`
									Detail string `json:"detail"`
								}
								_ = json.Unmarshal(data, &problem)
								result.Error = &clustersignal.StatusError{Status: resp.StatusCode, Code: problem.Code, Detail: problem.Detail}
							}
						}
					}
					if err != nil {
						result.Error = &clustersignal.StatusError{Status: 503, Detail: err.Error()}
					}
					s.Acknowledge(clustersignal.Acknowledgment{Incarnation: batch.Incarnation, Results: []clustersignal.Result{result}})
					mu.Lock()
					delete(seen, op.ID)
					mu.Unlock()
				}()
			}
		}
	}()
	t.Cleanup(func() { cancel(); s.Stop(context.Background()); wg.Wait() })
	return s
}
func (s *Server) handleTestNodeProviderEnabled(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), providerSyncWaitTimeout)
	defer cancel()
	if s.configStore == nil {
		ServiceUnavailable(c, "providers config store not ready")
		return
	}
	ep := s.certifiedWorker(peerCertFromRequest(c.Request))
	if ep == nil {
		Forbidden(c, "the client certificate does not name a worker of this cluster")
		return
	}
	node := ep.NodeName
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !BindJSONStrict(c, &req) {
		return
	}
	if req.Enabled == nil {
		BadRequest(c, "enabled is required")
		return
	}
	if err := s.recordNodeProviderEnabled(ctx, node, c.Param("name"), *req.Enabled); err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": c.Param("name"), "node": node, "enabled": *req.Enabled})
}

func (s *Server) handleTestCatalogNotice(c *gin.Context) {
	if s.node.IsWorker() {
		BadRequest(c, "workers don't have cache to refresh")
		return
	}
	ep := s.certifiedWorker(peerCertFromRequest(c.Request))
	if ep == nil {
		Forbidden(c, "the client certificate does not name a worker of this cluster")
		return
	}
	slog.Info("Received worker cache refresh notice", "node", ep.NodeName)
	s.RefreshClusterEndpoint(ep.URL)
	if err := s.model.Cache.RefreshNode(c.Request.Context(), ep.NodeName); err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Cache refreshed successfully"})
}

// buildTestCoordinatorEngine supplies existing domain fixtures' local HTTP adapters.
// Production has no coordinator-facing worker API; real transport tests use typed operations.
func buildTestCoordinatorEngine(s *Server) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), modelAdmissionMiddleware())
	group := engine.Group("/zzrouter/v1/internal", RequestIDMiddleware(), APIVersionMiddleware())
	group.POST("/models/refresh", s.handleTestCatalogNotice)
	group.PUT("/providers/:name/enabled", s.handleTestNodeProviderEnabled)
	group.POST("/goodbye", func(c *gin.Context) {
		ep := s.certifiedWorker(peerCertFromRequest(c.Request))
		if ep == nil {
			Forbidden(c, "unknown worker")
			return
		}
		s.MarkClusterEndpointDown(ep.URL)
		c.JSON(http.StatusOK, gin.H{"status": "success"})
	})
	return engine
}
