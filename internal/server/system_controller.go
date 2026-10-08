package server

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/logging"
)

// SystemController handles HTTP requests for system-related operations.
type SystemController struct {
	service    *SystemService
	nodeConfig *pkgConfig.NodeConfig
	logManager *logging.Manager // nil when provider app manager is not available
}

// NewSystemController creates a new system controller.
func NewSystemController(service *SystemService, nodeConfig *pkgConfig.NodeConfig, logManager *logging.Manager) *SystemController {
	return &SystemController{
		service:    service,
		nodeConfig: nodeConfig,
		logManager: logManager,
	}
}

// RegisterPublicRoutes registers public system routes.
func (ctrl *SystemController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/system", ctrl.GetSystemInfo)
	router.GET("/server/identity", ctrl.ShowLocal)
	router.GET("/server/logs", ctrl.ListLogs)
}

// GetSystemInfo handles GET /zzrouter/v1/system — cluster-wide system info.
func (ctrl *SystemController) GetSystemInfo(c *gin.Context) {
	req := &GetSystemInfoRequest{Node: QueryNode(c)}

	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterActionTimeout)
	defer cancel()

	resp, err := ctrl.service.GetSystemInfo(ctx, req)
	if err != nil {
		RespondToError(c, err)
		return
	}

	respondSuccess(c, "System info retrieved", resp.System)
}

// ShowLocal handles GET /zzrouter/v1/server/identity — local node identity.
//
// cluster_mode and config_path answer "which config is this process
// actually running?". They are the diagnosis for a node whose CLI and
// server read different files, which happens whenever the server runs
// as a service under another account: both are configured, neither is
// wrong, and they disagree about what this node is.
func (ctrl *SystemController) ShowLocal(c *gin.Context) {
	id := nodeIdentityReportFrom(ctrl.nodeConfig)
	respondSuccess(c, "Local node identity", gin.H{
		"server":       id.Name,
		"cluster":      id.Mode != string(pkgConfig.ClusterModeDisabled) && id.Mode != string(pkgConfig.ClusterModeStandalone),
		"cluster_mode": id.Mode,
		"config_path":  id.ConfigPath,
		"endpoint":     fmt.Sprintf("%s:%d", id.Name, ctrl.nodeConfig.Node.Port),
	})
}

// ListLogs handles GET /zzrouter/v1/server/logs — list available instance log files.
func (ctrl *SystemController) ListLogs(c *gin.Context) {
	if ctrl.logManager == nil {
		ServiceUnavailable(c, "Log manager not available")
		return
	}

	logFiles, err := ctrl.logManager.ListLogFiles()
	if err != nil {
		InternalNodeError(c, "Failed to list log files: "+err.Error())
		return
	}

	logs := make([]gin.H, 0, len(logFiles))
	for _, lf := range logFiles {
		logs = append(logs, gin.H{
			"path":          lf.Path,
			"instance_id":   lf.InstanceID,
			"size":          lf.Size,
			"modified":      lf.ModTime.Format(time.RFC3339),
			"is_compressed": lf.IsCompressed,
		})
	}

	pagination, ok := ParsePagination(c)
	if !ok {
		return
	}
	paginated, total, hasMore := ApplyPagination(logs, pagination)
	respondList(c, paginated, total, hasMore)
}
