package server

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
)

// KeysController handles HTTP requests for virtual key management.
type KeysController struct {
	service *KeysService
}

// NewKeysController creates a new keys controller.
func NewKeysController(service *KeysService) *KeysController {
	return &KeysController{service: service}
}

// RegisterPublicRoutes registers key management routes on the public API.
func (ctrl *KeysController) RegisterPublicRoutes(router *gin.RouterGroup) {
	// Gin's radix tree prefers static segments, but registering /keys/schema
	// before /keys/:id keeps the precedence visible to grep.
	router.GET("/keys/schema", handleKeysSchema)
	router.POST("/keys", ctrl.CreateKey)
	router.GET("/keys", ctrl.ListKeys)
	router.GET("/keys/:id", ctrl.GetKey)
	router.PATCH("/keys/:id", ctrl.UpdateKey)
	router.DELETE("/keys/:id", ctrl.DeleteKey)
	router.POST("/keys/:id/rotate", ctrl.RotateKey)
	router.GET("/keys/:id/usage", ctrl.GetKeyUsage)
	router.POST("/keys/:id/usage/reset", ctrl.ResetKeyUsage)
	router.POST("/keys/reload", ctrl.ReloadKeys)
	router.GET("/spend/report", ctrl.GetSpendReport)
}

// createKeyHTTPRequest is the wire shape for POST /keys — service-layer
// CreateKeyRequest plus the URL-side id field. Lifted into a named type
// so the access-schema drift test asserts against the same shape gin
// binds against.
type createKeyHTTPRequest struct {
	ID string `json:"id" binding:"required"`
	CreateKeyRequest
}

// CreateKey handles POST /zzrouter/v1/keys
func (ctrl *KeysController) CreateKey(c *gin.Context) {
	var req createKeyHTTPRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	resp, err := ctrl.service.CreateKey(req.ID, &req.CreateKeyRequest, callerKeyID(c))
	if err != nil {
		if isConflict(err) {
			Conflict(c, err.Error())
			return
		}
		if isInvalidInput(err) {
			BadRequest(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	respondCreatedWithMessage(c, "Key created successfully. Save the key value: it will not be shown again.", resp)
}

// ListKeys handles GET /zzrouter/v1/keys
func (ctrl *KeysController) ListKeys(c *gin.Context) {
	result := ctrl.service.ListKeys()
	respondList(c, result, len(result), false)
}

// GetKey handles GET /zzrouter/v1/keys/:id
func (ctrl *KeysController) GetKey(c *gin.Context) {
	id := c.Param("id")

	resp, err := ctrl.service.GetKey(id)
	if err != nil {
		NotFound(c, err.Error())
		return
	}

	respondSuccess(c, "Key retrieved successfully", resp)
}

// UpdateKey handles PATCH /zzrouter/v1/keys/:id
func (ctrl *KeysController) UpdateKey(c *gin.Context) {
	id := c.Param("id")

	var req UpdateKeyRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	resp, err := ctrl.service.UpdateKey(id, &req, callerKeyID(c))
	if err != nil {
		if isNotFound(err) {
			NotFound(c, err.Error())
			return
		}
		if isInvalidInput(err) {
			BadRequest(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	respondSuccess(c, "Key updated successfully", resp)
}

// DeleteKey handles DELETE /zzrouter/v1/keys/:id
func (ctrl *KeysController) DeleteKey(c *gin.Context) {
	id := c.Param("id")

	if err := ctrl.service.DeleteKey(id, auditActor(c)); err != nil {
		if isNotFound(err) {
			NotFound(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	respondSuccess(c, "Key deleted successfully", nil)
}

// RotateKey handles POST /zzrouter/v1/keys/:id/rotate
func (ctrl *KeysController) RotateKey(c *gin.Context) {
	id := c.Param("id")

	resp, err := ctrl.service.RotateKey(id, auditActor(c))
	if err != nil {
		if isNotFound(err) {
			NotFound(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	respondSuccess(c, "Key rotated successfully. Save the new key value: it will not be shown again.", resp)
}

// ReloadKeys handles POST /zzrouter/v1/keys/reload
func (ctrl *KeysController) ReloadKeys(c *gin.Context) {
	if err := ctrl.service.ReloadKeys(); err != nil {
		// Surface the actual reason (e.g. file parse error, missing file) so
		// operators can fix it without tailing logs. SanitizeErrorMessage
		// inside InternalNodeError scrubs paths and stack traces.
		InternalNodeError(c, "failed to reload keys: "+err.Error())
		return
	}

	respondSuccess(c, "Keys reloaded successfully", nil)
}

// GetKeyUsage handles GET /zzrouter/v1/keys/:id/usage.
func (ctrl *KeysController) GetKeyUsage(c *gin.Context) {
	id := c.Param("id")

	resp, err := ctrl.service.GetKeyUsage(id)
	if err != nil {
		if isNotFound(err) {
			NotFound(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}
	respondSuccess(c, "Key usage retrieved", resp)
}

// ResetKeyUsage handles POST /zzrouter/v1/keys/:id/usage/reset.
func (ctrl *KeysController) ResetKeyUsage(c *gin.Context) {
	id := c.Param("id")

	if err := ctrl.service.ResetKeyUsage(id, auditActor(c)); err != nil {
		if isNotFound(err) {
			NotFound(c, err.Error())
			return
		}
		InternalNodeError(c, err.Error())
		return
	}
	respondSuccess(c, "Key usage reset successfully", nil)
}

// GetSpendReport handles GET /zzrouter/v1/spend/report.
// Optional query params:
//
//	key_id=<id> — restrict to a single key.
func (ctrl *KeysController) GetSpendReport(c *gin.Context) {
	resp := ctrl.service.GetSpendReport()
	if keyID := c.Query("key_id"); keyID != "" {
		filtered := resp.Keys[:0]
		var total float64
		for _, k := range resp.Keys {
			if k.KeyID == keyID {
				filtered = append(filtered, k)
				total += k.SpendUSD
			}
		}
		resp.Keys = filtered
		resp.TotalSpend = total
	}
	respondSuccess(c, "Spend report retrieved", resp)
}

func isNotFound(err error) bool {
	return errors.Is(err, keys.ErrKeyNotFound)
}

func isConflict(err error) bool {
	return errors.Is(err, keys.ErrKeyExists)
}
