package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/observability/logger"
)

// ============================================================================
// Params Controller — v5 surface (plan §7.3, coordinator-only writes)
// ============================================================================
//
// GET   /providers/:name/resolved    — resolved view + ETag header
// GET   /providers/:name/schema      — merged Go + YAML schema
// PATCH /providers/:name/parameters  — RFC 7396 Merge-Patch (single writer)
//
// Legacy GET shims (/parameters, /nodes/parameters, /models/parameters)
// stay alive until the TUI (plan step 10) migrates to /resolved.
// PUT/DELETE and PATCH-ignore variants return 410 Gone (CodeRetired) —
// mounted so clients see the retirement instead of a 404.

// ParamsController handles HTTP requests for parameter operations.
type ParamsController struct {
	executor *ParamsExecutor
}

// NewParamsController creates a new params controller.
func NewParamsController(executor *ParamsExecutor) *ParamsController {
	return &ParamsController{executor: executor}
}

// RegisterReadOnlyRoutes registers the subset of parameter routes that
// are safe on every node, including workers (plan §5.3: reads continue
// against the cached tree even when the coordinator is unreachable).
// The PATCH is mounted too so HandleParametersPatch's rejectOnWorker
// can emit the plan §5.1 421 Misdirected Request + Location header —
// without this, the method would router-level 405 with no redirect
// hint. Writes that hit a coordinator pass through rejectOnWorker
// unchanged and mutate normally.
func (ctrl *ParamsController) RegisterReadOnlyRoutes(router *gin.RouterGroup) {
	router.GET("/providers/:name/resolved", ctrl.executor.HandleResolved)
	router.GET("/providers/:name/schema", ctrl.executor.HandleSchema)
	// Legacy GET shims — backed by Resolve(), kept until TUI migrates.
	router.GET("/providers/:name/parameters", ctrl.executor.HandleInternalGetAppParameters)
	router.GET("/providers/:name/nodes/parameters", ctrl.executor.HandleInternalGetNodeParameters)
	router.GET("/providers/:name/models/parameters", ctrl.executor.HandleInternalGetModelParameters)
	router.PATCH("/providers/:name/parameters", ctrl.executor.HandleParametersPatch)
}

// RegisterPublicRoutes registers the v5 parameter routes + legacy shims.
// The read-only group covers resolved/schema/GET shims AND the PATCH
// (which self-rejects on workers via HandleParametersPatch). Coord
// mounts add the retired 410 handlers so agents predating the PATCH
// surface see the retirement instead of a 404.
func (ctrl *ParamsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	ctrl.RegisterReadOnlyRoutes(router)

	// Retired mutators — 410 with CodeRetired so agents see the new endpoint.
	router.PUT("/providers/:name/parameters", ctrl.executor.RetiredHandler)
	router.DELETE("/providers/:name/parameters/:key", ctrl.executor.RetiredHandler)
	router.PATCH("/providers/:name/parameters/:key/ignore", ctrl.executor.RetiredHandler)
	router.PUT("/providers/:name/nodes/parameters", ctrl.executor.RetiredHandler)
	router.DELETE("/providers/:name/nodes/parameters/:key", ctrl.executor.RetiredHandler)
	router.PATCH("/providers/:name/nodes/parameters/:key/ignore", ctrl.executor.RetiredHandler)
	router.PUT("/providers/:name/models/parameters", ctrl.executor.RetiredHandler)
	router.DELETE("/providers/:name/models/parameters/:key", ctrl.executor.RetiredHandler)
	router.PATCH("/providers/:name/models/parameters/:key/ignore", ctrl.executor.RetiredHandler)

	// Service management (unrelated to params refactor). The routing
	// variants, not the Internal ones: the public surface honours ?node=,
	// the internal surface always answers for the node it runs on.
	router.GET("/providers/:name/service/status", ctrl.executor.HandleServiceStatus)
	router.POST("/providers/:name/service/apply", ctrl.executor.HandleApplyService)
	for _, action := range []string{"start", "stop", "restart"} {
		router.POST("/providers/:name/service/"+action, ctrl.executor.HandleServiceControl(action))
	}

	logger.Debug("[ParamsController] Registered v5 parameter routes")
}
