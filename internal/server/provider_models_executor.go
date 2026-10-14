package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/utils"
)

func (e *DeploymentsExecutor) modelStoreConfig(provider string) (config.ServiceConfig, error) {
	cfg, ok := e.appsConfig().LookupApp(provider)
	if !ok {
		return cfg, provapps.ErrProviderNotFound
	}
	return cfg, nil
}

// HandleInternalListProviderModels reads local files without running the engine.
func (e *DeploymentsExecutor) HandleInternalListProviderModels(c *gin.Context) {
	provider := c.Param("name")
	cfg, err := e.modelStoreConfig(provider)
	if err != nil {
		RespondToError(c, err)
		return
	}
	categories, err := provapps.ListModelStore(c.Request.Context(), cfg, c.Query("category"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, providerModelList{Provider: provider, Node: e.getNodename(), Categories: categories})
}

// HandleInternalDeleteProviderModel shares the download admission reservation.
func (e *DeploymentsExecutor) HandleInternalDeleteProviderModel(c *gin.Context) {
	provider, category, name := c.Param("name"), c.Query("category"), c.Query("name")
	cfg, err := e.modelStoreConfig(provider)
	if err != nil {
		RespondToError(c, err)
		return
	}
	root, err := provapps.ModelStoreCategoryPath(cfg, category)
	if err != nil {
		RespondToError(c, err)
		return
	}
	boundary, err := utils.ResolvePath(fsroot.ProviderDir(cfg.Name))
	if err != nil {
		RespondToError(c, err)
		return
	}
	path, err := provapps.ModelStoreFilePath(boundary, root, name, false)
	if err != nil {
		RespondToError(c, err)
		return
	}
	e.jobKeysMu.Lock()
	defer e.jobKeysMu.Unlock()
	if e.jobs == nil || e.downloads == nil {
		ServiceUnavailable(c, "download job registry unavailable")
		return
	}
	key := providerModelDownloadKey(e.getNodename(), path)
	status := e.downloads.GetDownloadStatus(key)
	if e.downloadReserved(key) || (status != nil && !modelregistry.IsTerminalStatus(status.Status)) {
		RespondToError(c, httperr.Error{Status: http.StatusConflict, Code: string(httperr.CodeDownloadInProgress), Message: "provider model download is in progress"})
		return
	}
	if err := provapps.DeleteModelStoreFile(boundary, root, name); err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, providerModelDelete{Provider: provider, Node: e.getNodename(), Category: category, Name: name, Deleted: true})
}
