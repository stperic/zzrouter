package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

func (ctrl *ProvidersController) DeleteDisposable(c *gin.Context) {
	result, err := ctrl.providersService.DeleteDisposable(c.Request.Context(), c.Param("name"), c.Query("runtime"), c.Param("plan_id"), QueryNode(c))
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Disposable runtime removed", result)
}

func (s *ProvidersService) DeleteDisposable(ctx context.Context, provider, runtime, identity, node string) (json.RawMessage, error) {
	ctx, cancel := ensureTimeout(ctx, LongRequestTimeout)
	defer cancel()
	path := fmt.Sprintf("/zzrouter/v1/internal/providers/%s/install/disposable/%s", url.PathEscape(provider), url.PathEscape(identity))
	if runtime != "" {
		path += "?runtime=" + url.QueryEscape(runtime)
	}
	return s.routeRaw(ctx, "DELETE", path, localIfEmpty(node), nil)
}

func (e *ProvidersExecutor) HandleInternalDeleteDisposable(c *gin.Context) {
	if e.mgr == nil {
		InternalNodeError(c, "provider manager unavailable")
		return
	}
	if err := e.mgr.Install().DeleteDisposable(c.Request.Context(), c.Param("name"), c.Query("runtime"), c.Param("plan_id")); err != nil {
		respondRecipeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"removed": true, "plan_id": c.Param("plan_id")})
}
