package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/update"
)

// UpdateController resolves the scheduler lazily because routes precede startup.
type UpdateController struct {
	schedulerOf func() *update.Scheduler
	server      *Server
}

// NewUpdateController creates an always-registered update surface.
func NewUpdateController(schedulerOf func() *update.Scheduler) *UpdateController {
	if schedulerOf == nil {
		schedulerOf = func() *update.Scheduler { return nil }
	}
	return &UpdateController{
		schedulerOf: schedulerOf,
	}
}

// scheduler resolves the live scheduler pointer at request time.
func (c *UpdateController) scheduler() *update.Scheduler {
	return c.schedulerOf()
}

// RegisterRoutes registers admin update reads and mutations.
func (c *UpdateController) RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/update/status", c.HandleGetStatus)
	router.POST("/update/check", c.HandleCheckNow)
	router.POST("/update/apply", c.HandleApplyNow)
	router.POST("/update/rollback", c.HandleRollback)
	router.GET("/update/history", c.HandleGetHistory)
	router.GET("/update/settings", c.HandleUpdateSettings)
	router.PATCH("/update/settings", c.HandleUpdateSettings)
}

// requireScheduler allows manual control even when scheduled updates are off.
func (c *UpdateController) requireScheduler(ctx *gin.Context) *update.Scheduler {
	sched := c.scheduler()
	if sched == nil {
		ServiceUnavailable(ctx, "update system is not initialized; retry after node startup")
		return nil
	}
	return sched
}

// HandleGetStatus returns the current update status
// GET /zzrouter/update/status
func (c *UpdateController) HandleGetStatus(ctx *gin.Context) {
	sched := c.requireScheduler(ctx)
	if sched == nil {
		return
	}
	if c.clusterRead(ctx, false) {
		return
	}
	respondSuccess(ctx, "Update status retrieved", sched.GetStatus())
}

// HandleCheckNow triggers an immediate update check
// POST /zzrouter/update/check
func (c *UpdateController) HandleCheckNow(ctx *gin.Context) {
	sched := c.requireScheduler(ctx)
	if sched == nil {
		return
	}
	result, err := sched.CheckNow(ctx.Request.Context())
	if err != nil {
		InternalNodeError(ctx, "update check failed: "+err.Error())
		return
	}
	respondSuccess(ctx, "Update check complete", gin.H{
		"update_available": result.UpdateAvailable,
		"current_version":  result.CurrentVersion.String(),
		"latest_release":   result.LatestRelease,
		"skipped_reason":   result.SkippedReason,
	})
}

// HandleApplyNow dispatches a pending update in the background and
// returns one serial rollout job or a local pending-update job.
// The scheduler owns accepted work after the request ends.
// POST /zzrouter/update/apply
func (c *UpdateController) HandleApplyNow(ctx *gin.Context) {
	var req updateApplyRequest
	if !BindJSONStrictOptional(ctx, &req) {
		return
	}
	if req.Version != nil || req.Nodes != nil {
		if req.Version == nil {
			BadRequest(ctx, "version is required for a node update")
			return
		}
		if err := update.ValidateVersion(*req.Version); err != nil {
			BadRequest(ctx, err.Error())
			return
		}
		if c.server == nil || c.server.updateRollouts == nil {
			ServiceUnavailable(ctx, "cluster update control is not initialized")
			return
		}
		nodes, err := c.server.selectUpdateNodes(req.Nodes)
		if err != nil {
			BadRequest(ctx, err.Error())
			return
		}
		record, err := c.server.updateRollouts.Submit(*req.Version, nodes)
		if err != nil {
			if errors.Is(err, update.ErrApplyInFlight) {
				RespondToError(ctx, newProblemError(http.StatusConflict, "Conflict", err.Error()))
			} else {
				InternalNodeError(ctx, err.Error())
			}
			return
		}
		respondAccepted(ctx, "Node update job queued. Poll /zzrouter/v1/jobs/:id or /zzrouter/v1/update/status?nodes=all.", record)
		return
	}
	sched := c.requireScheduler(ctx)
	if sched == nil {
		return
	}
	if sched.Delegated() != nil {
		c.delegate(ctx, sched, update.ActionApply,
			"Update handed to the privileged updater. Poll GET /zzrouter/v1/update/status for progress.")
		return
	}
	jobID, err := sched.ApplyNowAsync(ctx.Request.Context())
	if err != nil {
		if errors.Is(err, update.ErrNoUpdateAvailable) {
			RespondToError(ctx, newProblemError(http.StatusConflict, "Conflict",
				"no update is available; run POST /zzrouter/v1/update/check first"))
			return
		}
		InternalNodeError(ctx, "update apply failed: "+err.Error())
		return
	}
	data := gin.H{}
	if jobID != "" {
		data["job_id"] = jobID
	}
	respondAccepted(ctx, "Update apply dispatched. Subscribe to /zzrouter/v1/jobs/:id/stream for progress.", data)
}

// HandleRollback rolls back to the previous version
// POST /zzrouter/update/rollback
func (c *UpdateController) HandleRollback(ctx *gin.Context) {
	sched := c.requireScheduler(ctx)
	if sched == nil {
		return
	}
	if sched.Delegated() != nil {
		c.delegate(ctx, sched, update.ActionRollback,
			"Rollback handed to the privileged updater. Poll GET /zzrouter/v1/update/status for progress.")
		return
	}
	result, err := sched.Rollback(ctx.Request.Context())
	if err != nil {
		if errors.Is(err, update.ErrNoBackupsAvailable) {
			NotFound(ctx, err.Error())
			return
		}
		InternalNodeError(ctx, "rollback failed: "+err.Error())
		return
	}
	if !result.Success {
		InternalNodeError(ctx, "rollback failed: "+result.Error)
		return
	}
	respondSuccess(ctx, "Rollback successful. Manual restart required.", gin.H{
		"backup_path": result.BackupPath,
	})
}

// HandleGetHistory returns the update history
// GET /zzrouter/update/history
func (c *UpdateController) HandleGetHistory(ctx *gin.Context) {
	sched := c.requireScheduler(ctx)
	if sched == nil {
		return
	}
	if c.clusterRead(ctx, true) {
		return
	}
	history, err := sched.GetHistory()
	if err != nil {
		InternalNodeError(ctx, "failed to retrieve update history: "+err.Error())
		return
	}
	ctx.JSON(http.StatusOK, history)
}

// delegate hands an action to the privileged updater and answers 202.
//
// A different answer from the same route on a node that installs its
// own updates, and deliberately so: nothing has been applied yet, and
// there is no job to stream because the work runs in a process this one
// does not own. What the caller gets is an acknowledgement plus where to
// watch for the outcome.
func (c *UpdateController) delegate(ctx *gin.Context, sched *update.Scheduler, action update.RequestAction, message string) {
	if err := sched.RequestPrivileged(action, requesterOf(ctx)); err != nil {
		InternalNodeError(ctx, "could not reach the privileged updater: "+err.Error())
		return
	}
	respondAccepted(ctx, message, gin.H{
		"delegated_to": sched.Delegated().RequestPath(),
		"action":       string(action),
	})
}

// requesterOf names who asked, for the updater's log. Identification
// only: the privileged side makes no decision from it.
func requesterOf(ctx *gin.Context) string {
	if id := ctx.GetString("request_id"); id != "" {
		return "api request " + id
	}
	return "api"
}
