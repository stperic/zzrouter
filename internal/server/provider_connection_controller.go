package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ProviderConnectionController is an admin-only adapter on coordinator routes.
type ProviderConnectionController struct{ service *ProviderConnectionService }

func (ctrl *ProviderConnectionController) RegisterPublicRoutes(group *gin.RouterGroup) {
	group.GET("/providers/:name/connection", ctrl.Status)
	group.POST("/providers/:name/connection/test", ctrl.Test)
	group.PUT("/providers/:name/connection", ctrl.Save)
	group.DELETE("/providers/:name/connection", ctrl.Remove)
	group.POST("/providers/:name/connection/use-external", ctrl.UseExternal)
}

func (ctrl *ProviderConnectionController) Status(c *gin.Context) {
	if !connectionScope(c) {
		return
	}
	result, err := ctrl.service.Status(c.Param("name"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Provider connection retrieved", result)
}

func (ctrl *ProviderConnectionController) Test(c *gin.Context) {
	if !connectionScope(c) {
		return
	}
	var request struct {
		Values map[string]string `json:"values"`
	}
	if !bindConnection(c, &request) {
		return
	}
	result, err := ctrl.service.Test(c.Request.Context(), c.Param("name"), request.Values)
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Provider connection tested", result)
}

func (ctrl *ProviderConnectionController) Save(c *gin.Context) {
	if !connectionScope(c) {
		return
	}
	var request struct {
		ExpectedRevision *uint64           `json:"expected_revision"`
		Values           map[string]string `json:"values"`
		Activate         bool              `json:"activate"`
	}
	if !bindConnection(c, &request) {
		return
	}
	if request.ExpectedRevision == nil {
		BadRequest(c, "expected_revision is required")
		return
	}
	result, err := ctrl.service.Save(c.Request.Context(), c.Param("name"), *request.ExpectedRevision, request.Values, request.Activate)
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Provider connection saved", result)
}

func (ctrl *ProviderConnectionController) Remove(c *gin.Context)      { ctrl.changeOwnership(c, false) }
func (ctrl *ProviderConnectionController) UseExternal(c *gin.Context) { ctrl.changeOwnership(c, true) }
func (ctrl *ProviderConnectionController) changeOwnership(c *gin.Context, external bool) {
	if !connectionScope(c) {
		return
	}
	var request struct {
		ExpectedRevision *uint64 `json:"expected_revision"`
	}
	if !bindConnection(c, &request) {
		return
	}
	if request.ExpectedRevision == nil {
		BadRequest(c, "expected_revision is required")
		return
	}
	var result *ConnectionApplyResult
	var err error
	if external {
		result, err = ctrl.service.UseExternal(c.Request.Context(), c.Param("name"), *request.ExpectedRevision)
	} else {
		result, err = ctrl.service.Remove(c.Request.Context(), c.Param("name"), *request.ExpectedRevision)
	}
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Provider connection ownership updated", result)
}

func connectionScope(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if c.Query("node") != "" {
		BadRequest(c, "provider connections are coordinator-scoped; node selection is not supported")
		return false
	}
	return true
}

// Credential decoding deliberately never reflects an untrusted key or value.
func bindConnection(c *gin.Context, value any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(value)
	if err == nil {
		var trailing any
		if tailErr := decoder.Decode(&trailing); !errors.Is(tailErr, io.EOF) {
			err = errors.New("trailing JSON")
		}
	}
	if err == nil {
		return true
	}
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		RespondWithProblem(c, 413, "Request Entity Too Large", "connection request exceeds 65536 bytes")
	} else {
		BadRequest(c, "invalid connection request; send one JSON object with only declared fields")
	}
	return false
}
