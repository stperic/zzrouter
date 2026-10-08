// package server — TeamsController: HTTP surface for team management.
//
// Access control lives at the middleware layer (accessControlAuthMiddleware
// in routes.go). Every route below is gated to admin by construction — there
// are no in-handler role checks, and no "team owner self-service" narrowing.

package server

import (
	"encoding/json"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// TeamsController handles HTTP requests for team management.
type TeamsController struct {
	service *TeamsService
}

// NewTeamsController creates a new teams controller.
func NewTeamsController(service *TeamsService) *TeamsController {
	return &TeamsController{service: service}
}

// RegisterPublicRoutes registers team management routes.
func (ctrl *TeamsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	// Gin's radix tree prefers static segments, but registering /teams/schema
	// before /teams/:id keeps the precedence visible to grep.
	router.GET("/teams/schema", handleTeamsSchema)
	router.POST("/teams", ctrl.CreateTeam)
	router.GET("/teams", ctrl.ListTeams)
	router.GET("/teams/:id", ctrl.GetTeam)
	router.PATCH("/teams/:id", ctrl.UpdateTeam)
	router.DELETE("/teams/:id", ctrl.DeleteTeam)
	router.GET("/teams/:id/keys", ctrl.GetTeamKeys)
	router.GET("/teams/:id/usage", ctrl.GetTeamUsage)
	router.POST("/teams/:id/usage/reset", ctrl.ResetTeamUsage)
}

// ============================================================================
// Handlers
// ============================================================================

func (ctrl *TeamsController) CreateTeam(c *gin.Context) {
	var req CreateTeamRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	resp, err := ctrl.service.CreateTeam(&req, callerKeyID(c))
	if err != nil {
		writeTeamsError(c, err)
		return
	}

	respondCreatedWithMessage(c, "Team created successfully", resp)
}

func (ctrl *TeamsController) ListTeams(c *gin.Context) {
	result := ctrl.service.ListTeams()
	respondList(c, result, len(result), false)
}

func (ctrl *TeamsController) GetTeam(c *gin.Context) {
	resp, err := ctrl.service.GetTeam(c.Param("id"))
	if err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team retrieved successfully", resp)
}

// updateTeamHTTPRequest exists so `members` can be refused with a pointed
// error instead of the generic unknown-field 400.
//
// The API specification documents a members merge-patch
// the service never implemented — membership lives on the key
// (VirtualKey.TeamID), and this service mutates no member list because
// there isn't one. While binding was lenient the field was silently
// discarded and the rest of the patch applied; under strict binding it
// would take the rest of the patch down with it, so a caller following
// the documented example would lose a rename it never learned had failed.
type updateTeamHTTPRequest struct {
	UpdateTeamRequest
	Members json.RawMessage `json:"members,omitempty"`
}

func (ctrl *TeamsController) UpdateTeam(c *gin.Context) {
	var req updateTeamHTTPRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	if len(req.Members) > 0 {
		BadRequest(c, "members is not a team field: a key belongs to exactly one team, so membership is set on the key: "+
			"PATCH /zzrouter/v1/keys/{id} with {\"team_id\": \"<team>\"} to move a key into this team")
		return
	}
	resp, err := ctrl.service.UpdateTeam(c.Param("id"), &req.UpdateTeamRequest, callerKeyID(c))
	if err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team updated successfully", resp)
}

func (ctrl *TeamsController) DeleteTeam(c *gin.Context) {
	if err := ctrl.service.DeleteTeam(c.Param("id"), auditActor(c)); err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team deleted successfully", nil)
}

func (ctrl *TeamsController) GetTeamKeys(c *gin.Context) {
	resp, err := ctrl.service.GetTeamKeys(c.Param("id"))
	if err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team keys retrieved successfully", resp)
}

func (ctrl *TeamsController) GetTeamUsage(c *gin.Context) {
	resp, err := ctrl.service.GetTeamUsage(c.Param("id"))
	if err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team usage retrieved", resp)
}

func (ctrl *TeamsController) ResetTeamUsage(c *gin.Context) {
	if err := ctrl.service.ResetTeamUsage(c.Param("id"), auditActor(c)); err != nil {
		writeTeamsError(c, err)
		return
	}
	respondSuccess(c, "Team usage reset successfully", nil)
}

// ============================================================================
// Shared helpers
// ============================================================================

// callerKeyID returns the calling key's ID for CreatedBy / UpdatedBy audit
// fields. Returns "" if the access context is missing (synthetic bypass paths).
func callerKeyID(c *gin.Context) string {
	ac := GetAccessContext(c)
	if ac == nil || ac.Key == nil {
		return ""
	}
	return ac.Key.ID
}

// writeTeamsError translates service-layer errors into HTTP responses.
func writeTeamsError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, teams.ErrTeamNotFound):
		NotFound(c, err.Error())
	case errors.Is(err, teams.ErrTeamExists):
		Conflict(c, err.Error())
	case errors.Is(err, teams.ErrTeamHasMembers):
		Conflict(c, err.Error())
	case errors.Is(err, ErrPersonalTeamImmutable):
		Conflict(c, err.Error())
	case isInvalidInput(err):
		BadRequest(c, err.Error())
	default:
		InternalNodeError(c, err.Error())
	}
}
