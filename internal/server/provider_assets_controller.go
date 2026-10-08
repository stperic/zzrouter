package server

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ProviderAssetsController serves a provider's asset files on the
// coordinator, which owns them; peer-sync carries every change to the
// workers. Shipped assets belong to the release and cannot be replaced
// or removed here; operators add their own under other names.
type ProviderAssetsController struct {
	store *pkgConfig.AppsConfigStore
	// shipped reports whether the release owns an asset name.
	shipped func(provider, name string) bool
	// awaitSync and runs follow a write as they do a parameter write: the
	// workers get the asset, then the runs using it are reported.
	awaitSync providerSyncWait
	runs      *runsRefresher
}

// NewProviderAssetsController creates the controller. shipped answers
// which names belong to the release (templates.IsShippedAsset).
func NewProviderAssetsController(store *pkgConfig.AppsConfigStore, shipped func(provider, name string) bool) *ProviderAssetsController {
	return &ProviderAssetsController{store: store, shipped: shipped}
}

// WithSyncWait makes a write wait for the workers to have the asset.
func (ctrl *ProviderAssetsController) WithSyncWait(await providerSyncWait) *ProviderAssetsController {
	ctrl.awaitSync = await
	return ctrl
}

// WithRunsRefresher makes a write report the running models it affects,
// and restart them when asked.
func (ctrl *ProviderAssetsController) WithRunsRefresher(r *runsRefresher) *ProviderAssetsController {
	ctrl.runs = r
	return ctrl
}

// RegisterPublicRoutes wires /providers/:name/assets.
func (ctrl *ProviderAssetsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/providers/:name/assets", ctrl.List)
	router.GET("/providers/:name/assets/:asset", ctrl.Get)
	router.PUT("/providers/:name/assets/:asset", ctrl.Put)
	router.DELETE("/providers/:name/assets/:asset", ctrl.Delete)
}

// assetDTO describes one asset on the wire.
type assetDTO struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Shipped bool   `json:"shipped"`
	// ReferencedBy lists the parameters naming this asset, as merge-patch
	// paths ending in the key.
	ReferencedBy []string `json:"referenced_by"`
}

type assetListResponse struct {
	Assets []assetDTO `json:"assets"`
}

// List handles GET /providers/:name/assets.
func (ctrl *ProviderAssetsController) List(c *gin.Context) {
	provider := c.Param("name")
	dir, ok := ctrl.dir(c, provider)
	if !ok {
		return
	}
	infos, err := dir.List()
	if err != nil {
		respondAssetError(c, err)
		return
	}
	refs := ctrl.references(provider)
	out := assetListResponse{Assets: make([]assetDTO, 0, len(infos))}
	for _, info := range infos {
		out.Assets = append(out.Assets, ctrl.toDTO(provider, info, refs))
	}
	c.JSON(http.StatusOK, out)
}

// Get handles GET /providers/:name/assets/:asset with the raw bytes.
func (ctrl *ProviderAssetsController) Get(c *gin.Context) {
	dir, ok := ctrl.dir(c, c.Param("name"))
	if !ok {
		return
	}
	data, err := dir.Read(c.Param("asset"))
	if err != nil {
		respondAssetError(c, err)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

// Put handles PUT /providers/:name/assets/:asset: the raw body becomes
// the asset. 201 when new, 200 when replaced.
func (ctrl *ProviderAssetsController) Put(c *gin.Context) {
	provider, name := c.Param("name"), c.Param("asset")
	restart, ok := readRestart(c)
	if !ok || !ctrl.mutable(c, provider, name) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, assets.MaxAssetBytes)
	data, ok := readRequestBody(c)
	if !ok {
		return
	}
	created, err := ctrl.store.WriteAsset(provider, name, data)
	if isAssetCapError(err) && !errors.Is(err, assets.ErrStoredSet) {
		RespondWithProblem(c, http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge), err.Error())
		return
	}
	if err != nil {
		respondAssetError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	sync := ctrl.awaitSync.await(c)
	info := assets.Info{Name: name, Size: int64(len(data)), SHA256: assets.Digest(data)}
	c.JSON(status, assetWriteResponse{
		assetDTO: ctrl.toDTO(provider, info, ctrl.references(provider)),
		Runs:     ctrl.runs.report(c.Request.Context(), provider, sync, restart),
	})
}

// assetWriteResponse is an asset write's answer: the asset as stored, and
// what it means for the models already running.
type assetWriteResponse struct {
	assetDTO
	Runs *RunsReport `json:"runs,omitempty"`
}

// Delete handles DELETE /providers/:name/assets/:asset. An asset a
// parameter still names is refused, listing every such parameter, since
// removing it would make those launches fail.
func (ctrl *ProviderAssetsController) Delete(c *gin.Context) {
	provider, name := c.Param("name"), c.Param("asset")
	if !ctrl.mutable(c, provider, name) {
		return
	}
	sch := loadProviderSchema(ctrl.store, provider)
	err := ctrl.store.DeleteAsset(provider, name, func(cfg pkgConfig.ServiceConfig) []string {
		return assetReferences(&cfg, sch)[name]
	})
	var inUse *pkgConfig.AssetInUseError
	if errors.As(err, &inUse) {
		errs := make([]utils.ParamError, 0, len(inUse.Paths))
		for _, p := range inUse.Paths {
			errs = append(errs, utils.ParamError{Key: p, Code: string(httperr.CodeAssetInUse),
				Message: fmt.Sprintf("%s names asset %q", p, name)})
		}
		RespondWithParamErrors(c, http.StatusConflict, "Conflict", errs)
		return
	}
	if err != nil {
		respondAssetError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// dir returns the provider's asset directory, or writes why there is none.
func (ctrl *ProviderAssetsController) dir(c *gin.Context, provider string) (assets.Dir, bool) {
	if ctrl.store == nil {
		ServiceUnavailable(c, "providers config store not ready")
		return assets.Dir{}, false
	}
	dir, err := ctrl.store.Assets(provider)
	if err != nil {
		respondAssetError(c, err)
		return assets.Dir{}, false
	}
	return dir, true
}

// mutable checks that a write to name may proceed: a store, a valid
// name, and not one the release ships.
func (ctrl *ProviderAssetsController) mutable(c *gin.Context, provider, name string) bool {
	if _, ok := ctrl.dir(c, provider); !ok {
		return false
	}
	if err := assets.ValidateName(name); err != nil {
		respondAssetError(c, err)
		return false
	}
	if ctrl.shipped(provider, name) {
		Forbidden(c, fmt.Sprintf("asset %q ships with zzRouter and is restored on every start; add your own under another name", name))
		return false
	}
	return true
}

// references maps each asset name to the parameters that name it.
func (ctrl *ProviderAssetsController) references(provider string) map[string][]string {
	cfg, ok := ctrl.store.Config().LookupApp(provider)
	if !ok {
		return nil
	}
	return assetReferences(&cfg, loadProviderSchema(ctrl.store, provider))
}

// assetRef is one asset-typed parameter: its merge-patch path ending in
// the key, and the asset name it holds.
type assetRef struct {
	Path string
	Name string
}

// assetRefs lists every asset-typed parameter set anywhere in the tree,
// in path order. "auto" names no asset: it is how an operator tier turns
// off a lower tier's file and lets the engine use the model's own.
func assetRefs(cfg *pkgConfig.ServiceConfig, sch *schema.ProviderSchema) []assetRef {
	var refs []assetRef
	for _, site := range cfg.ParameterSites() {
		shapes := sch.ForEndpoint(site.Endpoint)
		for _, key := range slices.Sorted(maps.Keys(site.Parameters)) {
			if shapes[key].Kind == schema.ParamAsset && !pkgConfig.IsAuto(site.Parameters[key]) {
				refs = append(refs, assetRef{Path: site.Path + "." + key, Name: site.Parameters[key]})
			}
		}
	}
	return refs
}

// assetReferences maps each asset name to the paths of the parameters
// naming it.
func assetReferences(cfg *pkgConfig.ServiceConfig, sch *schema.ProviderSchema) map[string][]string {
	refs := map[string][]string{}
	for _, r := range assetRefs(cfg, sch) {
		refs[r.Name] = append(refs[r.Name], r.Path)
	}
	return refs
}

// danglingAssetRefs maps the path of every asset-typed parameter whose
// asset does not locate to the name it holds.
func danglingAssetRefs(cfg *pkgConfig.ServiceConfig, sch *schema.ProviderSchema, locate assetLocator) map[string]string {
	dangling := map[string]string{}
	for _, r := range assetRefs(cfg, sch) {
		if _, err := locate(r.Name); err != nil {
			dangling[r.Path] = r.Name
		}
	}
	return dangling
}

// newlyDangling refuses the references a write would leave pointing at
// no asset. Ones already dangling before it are not the write's doing,
// so they do not block an unrelated change.
func newlyDangling(before, after map[string]string) error {
	var errs paramErrors
	for _, path := range slices.Sorted(maps.Keys(after)) {
		if name := after[path]; before[path] != name {
			errs = append(errs, utils.ParamError{Key: path, Code: string(httperr.CodeUnknownAsset),
				Message: fmt.Sprintf("%s=%q names no asset of this provider", path, name)})
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// paramErrors carries per-key refusals out of a store mutation.
type paramErrors []utils.ParamError

func (e paramErrors) Error() string {
	msgs := make([]string, 0, len(e))
	for _, pe := range e {
		msgs = append(msgs, pe.Message)
	}
	return strings.Join(msgs, "; ")
}

func (ctrl *ProviderAssetsController) toDTO(provider string, info assets.Info, refs map[string][]string) assetDTO {
	referencedBy := refs[info.Name]
	if referencedBy == nil {
		referencedBy = []string{}
	}
	return assetDTO{
		Name:         info.Name,
		Size:         info.Size,
		SHA256:       info.SHA256,
		Shipped:      ctrl.shipped(provider, info.Name),
		ReferencedBy: referencedBy,
	}
}

// respondAssetError maps a pkg/config/assets or store error to a Problem.
// A cap breach here is the stored set's (a file placed by hand), not the
// request's, so it is a server-side 500 with the reason; Put answers its
// own body's breach as a 413.
func respondAssetError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, assets.ErrInvalidName):
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{
			Key: "asset", Code: string(httperr.CodeInvalidValue), Want: "asset name", Message: err.Error(),
		}})
	case errors.Is(err, pkgConfig.ErrProviderNotFound), errors.Is(err, assets.ErrNotFound), errors.Is(err, assets.ErrNotRegular):
		NotFound(c, err.Error())
	case errors.Is(err, assets.ErrNameCollision):
		Conflict(c, err.Error())
	default:
		InternalNodeError(c, err.Error())
	}
}

// isAssetCapError reports a size or count limit breach.
func isAssetCapError(err error) bool {
	return errors.Is(err, assets.ErrTooLarge) || errors.Is(err, assets.ErrProviderTooLarge) || errors.Is(err, assets.ErrTooMany)
}
