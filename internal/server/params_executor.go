package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/backend"
	configschema "github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils"
)

// envKeyRe enforces UPPER_SNAKE on env var keys at the PATCH edge; flag
// names are validated against the merged schema instead of a regex.
var envKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// paramKeyRe bounds flag-key characters for consumers of the /runs
// launch path, which still takes free-form parameter maps.
var paramKeyRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// validateParameterMaps checks parameter keys and values for invalid
// characters. Preserved for the /runs launch path which has not moved
// to the typed PATCH flow.
func validateParameterMaps(parameters, environment map[string]string) error {
	for k, v := range parameters {
		if !paramKeyRe.MatchString(k) {
			return fmt.Errorf("invalid parameter key %q: must contain only letters, digits, hyphens, underscores", k)
		}
		if err := validateEnvValue(k, v); err != nil {
			return err
		}
	}
	for k, v := range environment {
		if err := validateEnvKey(k); err != nil {
			return err
		}
		if err := validateEnvValue(k, v); err != nil {
			return err
		}
	}
	return nil
}

// validateEnvKey rejects env var keys that would be invalid shell names.
// Value validation (control chars) travels through validateEnvValue.
func validateEnvKey(k string) error {
	if !envKeyRe.MatchString(k) {
		return fmt.Errorf("invalid environment variable %q: must be uppercase letters, digits, underscores", k)
	}
	return nil
}

func validateEnvValue(key, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return fmt.Errorf("invalid character in value for %q: control characters not allowed", key)
		}
	}
	return nil
}

// ============================================================================
// Params Executor — client-facing parameter endpoints (coordinator-only)
// ============================================================================
//
// v5 surface (plan §7.3):
//
//   GET   /providers/:name/resolved?node=&model=  - resolved view + ETag
//   GET   /providers/:name/schema                 - merged Go + YAML schema
//   PATCH /providers/:name/parameters             - RFC 7396 Merge-Patch
//
// Legacy GET variants (/parameters, /nodes/parameters, /models/parameters)
// are preserved as thin shims over Resolve() until the TUI migrates to
// /resolved (plan step 10). Legacy PUT/DELETE/PATCH-ignore routes are
// retired — Merge-Patch is the single mutator.

// ParamsExecutor handles direct local parameter operations.
// All config mutations are serialized by the AppsConfigStore.
type ParamsExecutor struct {
	installAuthority func(context.Context, string, pkgConfig.ServiceConfig) error
	appsConfig       func() *pkgConfig.AppsConfig
	backend          *backend.Resolver
	configStore      *pkgConfig.AppsConfigStore
	nodename         func() string
	knownNodes       func() map[string]bool
	router           routing.Router
	services         providerServices
	protocol         func(provider string) (protocol.FullProvider, bool)
	httpClient       *http.Client
	isWorker         func() bool   // nil on standalone
	coordURL         func() string // coordinator base URL on workers; "" when unknown

	// awaitSync waits for the provider-tree fan-out to push this node's
	// config to the workers. Nil off the coordinator and in tests, where
	// a write reaches nobody else.
	awaitSync providerSyncWait

	// runs reports the running models a write affects. Nil where no
	// cluster runs API is wired.
	runs *runsRefresher

	// catalogWeights reports the node holding real weights of that name,
	// from the coordinator's catalog of every node's models. Nil where no
	// catalog is wired.
	catalogWeights func(ctx context.Context, name string) (node string, ok bool, err error)
}

// NewParamsExecutor creates a new params executor.
func NewParamsExecutor(
	appsConfig func() *pkgConfig.AppsConfig,
	configStore *pkgConfig.AppsConfigStore,
	nodename func() string,
	knownNodes func() map[string]bool,
	router routing.Router,
	services providerServices,
	protocol func(provider string) (protocol.FullProvider, bool),
	httpClient *http.Client,
	isWorker func() bool,
	coordURL func() string,
) *ParamsExecutor {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: constants.HTTPDefaultTimeout}
	}

	return &ParamsExecutor{
		appsConfig:  appsConfig,
		backend:     backend.NewResolver(appsConfig),
		configStore: configStore,
		nodename:    nodename,
		knownNodes:  knownNodes,
		router:      router,
		services:    services,
		protocol:    protocol,
		httpClient:  httpClient,
		isWorker:    isWorker,
		coordURL:    coordURL,
	}
}

// WithSyncWait makes a parameter write wait for the provider-tree
// fan-out before it answers, so its 200 describes a tree the workers
// have rather than one still on its way to them.
func (e *ParamsExecutor) WithSyncWait(await providerSyncWait) *ParamsExecutor {
	e.awaitSync = await
	return e
}

// WithRunsRefresher makes a parameter write report the running models it
// affects, and restart them when asked.
func (e *ParamsExecutor) WithRunsRefresher(r *runsRefresher) *ParamsExecutor {
	e.runs = r
	return e
}

// WithCatalog lets a parameter write refuse a variant name that real
// weights already carry.
func (e *ParamsExecutor) WithCatalog(weights func(ctx context.Context, name string) (node string, ok bool, err error)) *ParamsExecutor {
	e.catalogWeights = weights
	return e
}

// rejectOnWorker short-circuits write endpoints on workers with 421
// Misdirected Request + Location pointing to the coordinator (plan §5.1).
// Returns true when the request was rejected.
func (e *ParamsExecutor) rejectOnWorker(c *gin.Context) bool {
	if e.isWorker == nil || !e.isWorker() {
		return false
	}
	loc := ""
	if e.coordURL != nil {
		base := e.coordURL()
		if base != "" {
			loc = base + c.Request.URL.Path
		}
	}
	if loc != "" {
		c.Header("Location", loc)
	}
	RespondWithProblemOpts(c, http.StatusMisdirectedRequest, "Misdirected Request",
		"provider parameter writes are coordinator-only",
		ProblemOpts{Code: "coordinator_only"})
	return true
}

// ============================================================================
// GET /providers/:name/resolved — provenance view + ETag
// ============================================================================

// resolvedValueDTO is the wire shape for a single resolved value with
// provenance (plan §6.1). Tier is emitted as its stable string form.
type resolvedValueDTO struct {
	Value any    `json:"value"`
	Tier  string `json:"tier"`
	Node  string `json:"node,omitempty"`
	Model string `json:"model,omitempty"`
	// Pattern is the model key that supplied the value when it is not the
	// model's own name: a glob.
	Pattern string `json:"pattern,omitempty"`
	// SHA256 is the content of the asset an asset-typed value names, so
	// a caller sees which bytes a launch gets, not only which name.
	SHA256 string `json:"sha256,omitempty"`
}

// resolvedResponse is the body for GET /providers/:name/resolved.
type resolvedResponse struct {
	Install     map[string]pkgConfig.ResolvedInstall `json:"install,omitempty"`
	Parameters  map[string]resolvedValueDTO          `json:"parameters"`
	Environment map[string]resolvedValueDTO          `json:"environment"`
	// Request is the request-body defaults each request for the model
	// gets; a field the client sends wins. Absent when there are none.
	Request map[string]resolvedValueDTO `json:"request,omitempty"`
	// From is the base whose weights the model runs, when it is a variant.
	From string `json:"from,omitempty"`
}

// HandleResolved handles GET /providers/:name/resolved?node=&model=.
func (e *ParamsExecutor) HandleResolved(c *gin.Context) {
	provider := c.Param("name")
	node := c.Query("node")
	if node == "" && e.nodename != nil {
		node = e.nodename()
	}
	model := c.Query("model")

	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}

	digests := e.assetDigests(provider)
	resolved := cfg.Resolve(node, model)
	out := e.resolvedView(provider, resolved, digests)
	if cfg.Install != nil {
		out.Install = map[string]pkgConfig.ResolvedInstall{}
		for runtime := range cfg.Install.Runtimes {
			recipe, err := cfg.ResolveInstall(node, runtime)
			if err != nil {
				BadRequest(c, err.Error())
				return
			}
			out.Install[runtime] = recipe
		}
	}
	// A remote node's file presence is answered by that node over mTLS.
	if node != "" && e.nodename != nil && node != e.nodename() && e.router != nil {
		resp, err := e.router.Route(c.Request.Context(), &routing.Request{Method: http.MethodGet, Path: constants.ZZROUTERInternalApps + "/" + url.PathEscape(provider) + "/resolved?" + url.Values{"model": []string{model}}.Encode(), Node: node})
		if err != nil {
			InternalNodeError(c, "failed to resolve model features on target node")
			return
		}
		if etag := resp.Headers.Get("ETag"); etag != "" {
			c.Header("ETag", etag)
		}
		c.Data(resp.StatusCode, "application/json", resp.Body)
		return
	}
	if node == "" || e.nodename == nil || node == e.nodename() {
		features, err := prov_apps.LocalizeModelFeatures(*cfg, model, pkgConfig.FlattenParameters(resolved.Parameters))
		if err != nil {
			InternalNodeError(c, "failed to resolve model features")
			return
		}
		for key, source := range features.Sources {
			out.Parameters[key] = resolvedValueDTO{Value: features.Params[key], Tier: source, SHA256: features.Files[key]}
			digests["feature:"+key] = features.Params[key] + "\x00" + features.Files[key]
		}
	}
	e.writeETag(c, provider, digests)
	c.JSON(http.StatusOK, out)
}

// resolvedView is the wire shape of a resolved tree, with each
// asset-typed value's content digest.
func (e *ParamsExecutor) resolvedView(provider string, r pkgConfig.ResolvedParams, digests map[string]string) resolvedResponse {
	out := resolvedToDTO(r)
	if len(digests) == 0 {
		return out
	}
	shapes := loadProviderSchema(e.configStore, provider).Parameters
	for key, v := range out.Parameters {
		if name, ok := v.Value.(string); ok && shapes[key].Kind == schema.ParamAsset {
			v.SHA256 = digests[name]
			out.Parameters[key] = v
		}
	}
	return out
}

// assetDigests maps each of the provider's assets on this node to its
// content digest; empty when it has none or they cannot be read.
func (e *ParamsExecutor) assetDigests(provider string) map[string]string {
	digests := map[string]string{}
	if e.configStore == nil {
		return digests
	}
	var infos []assets.Info
	dir, err := e.configStore.Assets(provider)
	if err == nil {
		infos, err = dir.List()
	}
	if err != nil {
		// The view still answers without digests, so say why they are missing.
		slog.Warn("resolved view: provider assets unreadable, digests omitted", "provider", provider, "error", err)
		return digests
	}
	for _, info := range infos {
		digests[info.Name] = info.SHA256
	}
	return digests
}

// resolvedToDTO converts an internal ResolvedParams into the wire shape.
func resolvedToDTO(r pkgConfig.ResolvedParams) resolvedResponse {
	out := resolvedResponse{
		Parameters:  make(map[string]resolvedValueDTO, len(r.Parameters)),
		Environment: make(map[string]resolvedValueDTO, len(r.Environment)),
	}
	for k, v := range r.Parameters {
		out.Parameters[k] = resolvedValueDTO{Value: v.Value, Tier: v.Tier.String(), Node: v.Node, Model: v.Model, Pattern: v.Pattern}
	}
	for k, v := range r.Environment {
		out.Environment[k] = resolvedValueDTO{Value: v.Value, Tier: v.Tier.String(), Node: v.Node, Model: v.Model, Pattern: v.Pattern}
	}
	if len(r.Request) > 0 {
		out.Request = make(map[string]resolvedValueDTO, len(r.Request))
		for k, v := range r.Request {
			out.Request[k] = resolvedValueDTO{Value: v.Value, Tier: v.Tier.String(), Model: v.Model, Pattern: v.Pattern}
		}
	}
	out.From = r.From
	return out
}

// writeETag sets the ETag header from the on-disk config.yaml bytes and
// the provider's asset digests, since an asset's content is part of what
// a resolved value means. Best-effort — a read failure just omits the
// header rather than failing the request (ETag is advisory until
// If-Match lands in a later phase).
func (e *ParamsExecutor) writeETag(c *gin.Context, provider string, digests map[string]string) {
	if e.configStore == nil {
		return
	}
	data, err := e.configStore.ReadProviderConfigBytes(provider)
	if err != nil {
		return
	}
	h := sha256.New()
	h.Write(data)
	for _, name := range slices.Sorted(maps.Keys(digests)) {
		fmt.Fprintf(h, "\x00%s\x00%s", name, digests[name])
	}
	c.Header("ETag", `"sha256:`+hex.EncodeToString(h.Sum(nil))+`"`)
}

// ============================================================================
// GET /providers/:name/schema — merged Go + YAML schema
// ============================================================================

// schemaParamDTO mirrors schema.ParamShape on the wire (pointers become
// inline numeric fields; empty Kind becomes "string" at the DTO edge).
type schemaParamDTO struct {
	Type        string   `json:"type"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	// Pass is how an asset-typed value reaches the engine: its path on
	// the launching node, or its content as the flag's value.
	Pass string `json:"pass,omitempty"`
}

type endpointSchemaDTO struct {
	Parameters map[string]schemaParamDTO `json:"parameters"`
}

type schemaResponse struct {
	Install     json.RawMessage              `json:"install,omitempty"`
	Diagnostics *schema.Diagnostics          `json:"diagnostics,omitempty"`
	Parameters  map[string]schemaParamDTO    `json:"parameters"`
	Environment map[string]schemaParamDTO    `json:"environment"`
	Endpoints   map[string]endpointSchemaDTO `json:"endpoints,omitempty"`
}

// HandleSchema handles GET /providers/:name/schema. Unions the Go spine
// with schema.yaml on disk. Unknown provider → 404.
func (e *ParamsExecutor) HandleSchema(c *gin.Context) {
	provider := c.Param("name")
	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	merged := loadProviderSchema(e.configStore, provider)
	resp := schemaResponse{
		Diagnostics: merged.Diagnostics,
		Parameters:  paramShapeMapToDTO(merged.Parameters),
		Environment: make(map[string]schemaParamDTO),
	}
	if cfg.Install != nil {
		definition, err := configschema.InstallDefinition()
		if err != nil {
			InternalNodeError(c, err.Error())
			return
		}
		resp.Install = definition
	}
	if len(merged.Endpoints) > 0 {
		resp.Endpoints = make(map[string]endpointSchemaDTO, len(merged.Endpoints))
		for epName, ep := range merged.Endpoints {
			resp.Endpoints[epName] = endpointSchemaDTO{Parameters: paramShapeMapToDTO(ep.Parameters)}
		}
	}
	c.JSON(http.StatusOK, resp)
}

// paramShapeMapToDTO is shared by HandleSchema across the flat and
// per-endpoint param maps so wire shape stays identical.
func paramShapeMapToDTO(m map[string]schema.ParamShape) map[string]schemaParamDTO {
	out := make(map[string]schemaParamDTO, len(m))
	for name, shape := range m {
		kind := string(shape.Kind)
		if kind == "" {
			kind = "string"
		}
		out[name] = schemaParamDTO{
			Type:        kind,
			Min:         shape.Min,
			Max:         shape.Max,
			Description: shape.Description,
			Enum:        shape.Enum,
			Pass:        string(shape.Pass),
		}
	}
	return out
}

// ============================================================================
// PATCH /providers/:name/parameters — RFC 7396 Merge-Patch
// ============================================================================

// HandleParametersPatch handles PATCH /providers/:name/parameters.
func (e *ParamsExecutor) HandleParametersPatch(c *gin.Context) {
	if e.rejectOnWorker(c) {
		return
	}
	provider := c.Param("name")
	restart, ok := readRestart(c)
	if !ok {
		return
	}

	if ct := c.GetHeader("Content-Type"); ct != "" && ct != "application/merge-patch+json" &&
		ct != "application/json" {
		BadRequest(c, "Content-Type must be application/merge-patch+json")
		return
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		BadRequest(c, "read body: "+err.Error())
		return
	}
	patch, ok := decodePatchBody(c, raw)
	if !ok {
		return
	}

	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}

	mergedSchema := loadProviderSchema(e.configStore, provider)
	nodes := map[string]bool{}
	if e.knownNodes != nil {
		nodes = e.knownNodes()
	}
	// The coordinator itself must always be a valid target.
	if local := e.nodename(); local != "" {
		nodes[local] = true
	}
	var catalogErr error
	var weights func(string) (string, bool)
	if e.catalogWeights != nil {
		weights = func(name string) (string, bool) {
			node, found, err := e.catalogWeights(c.Request.Context(), name)
			if err != nil {
				catalogErr = err
			}
			return node, found
		}
	}
	errs := validatePatch(provider, cfg, patch, mergedSchema, patchScope{nodes: nodes, weights: weights})
	errs = append(errs, validateInstallPatch(patch, *cfg, restart)...)
	if catalogErr != nil {
		ServiceUnavailable(c, "cannot validate variant names: "+catalogErr.Error())
		return
	}
	if len(errs) > 0 {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", errs)
		return
	}

	// Forensic audit (plan §7.1): actor key + body hash on the request log.
	bodySum := sha256.Sum256(raw)
	slog.Info("[ParamsExecutor] parameters merge-patch",
		"provider", provider,
		"actor.key_id", PrincipalFromContext(c),
		"body_sha256", hex.EncodeToString(bodySum[:]),
	)

	locate := providerAssets(e.configStore, provider)
	if err := e.configStore.ApplyParameterPatch(provider, func(sc *pkgConfig.ServiceConfig) error {
		// Under the lock asset deletes take, so the assets checked here
		// are the ones the stored config will name.
		before := danglingAssetRefs(sc, mergedSchema, locate)
		original, err := cloneService(sc)
		if err != nil {
			return err
		}
		if err := applyPatchToConfig(sc, patch); err != nil {
			return err
		}
		if err := e.checkInstallMutation(c.Request.Context(), &original, sc, nodes); err != nil {
			return err
		}
		return newlyDangling(before, danglingAssetRefs(sc, mergedSchema, locate))
	}); err != nil {
		if errors.Is(err, pkgConfig.ErrProviderNotFound) {
			NotFound(c, err.Error())
			return
		}
		var refused paramErrors
		if errors.As(err, &refused) {
			RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", refused)
			return
		}
		var problems pkgConfig.ModelProblems
		if errors.As(err, &problems) {
			RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", modelProblemErrors(problems))
			return
		}
		InternalNodeError(c, err.Error())
		return
	}

	// A worker launches from its own copy of this tree, so answering
	// before the push lands would promise a value the very next launch
	// cannot read — the store's listeners have already started the
	// fan-out by the time the mutation returns, so this waits on it
	// rather than starting anything.
	sync := e.awaitSync.await(c)
	runs := e.runs.report(c.Request.Context(), provider, sync, restart)

	// Respond with refreshed resolved view + new ETag.
	cfg2, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		InternalNodeError(c, "failed to reload configuration")
		return
	}
	digests := e.assetDigests(provider)
	e.writeETag(c, provider, digests)
	// Default response view keys off the patch's most-specific tier when
	// callers skip ?node=&model=; fall back to coordinator node name.
	patchNode, patchModel := patchView(patch)
	node := cmp.Or(c.Query("node"), patchNode, e.nodename())
	model := cmp.Or(c.Query("model"), patchModel)
	view := e.resolvedView(provider, cfg2.Resolve(node, model), digests)
	if cfg2.Install != nil {
		view.Install = map[string]pkgConfig.ResolvedInstall{}
		for runtime := range cfg2.Install.Runtimes {
			resolved, resolveErr := cfg2.ResolveInstall(node, runtime)
			if resolveErr != nil {
				InternalNodeError(c, resolveErr.Error())
				return
			}
			view.Install[runtime] = resolved
		}
	}
	c.JSON(http.StatusOK, parametersWriteResponse{
		resolvedResponse: view,
		Runs:             runs,
	})
}

// patchView names the one node and the one model a patch wrote, where it
// wrote only one, so the answer shows the cell written rather than the
// provider defaults beside it.
func patchView(p *paramPatchBody) (node, model string) {
	if len(p.Models) == 1 {
		for m := range p.Models {
			model = m
		}
	}
	if len(p.Nodes) == 1 {
		for n, np := range p.Nodes {
			node = n
			if model == "" && np != nil && len(np.Models) == 1 {
				for m := range np.Models {
					model = m
				}
			}
		}
	}
	// A glob key names no model to resolve; resolving it literally would
	// miss the very cell it wrote.
	if strings.ContainsAny(model, globMetaChars) {
		model = ""
	}
	return node, model
}

// globMetaChars are the filepath.Match metacharacters a model key may use.
const globMetaChars = "*?["

// modelProblemErrors reports what the patched tree could not hold, each
// at the path of the field at fault.
func modelProblemErrors(problems pkgConfig.ModelProblems) []utils.ParamError {
	errs := make([]utils.ParamError, len(problems))
	for i, p := range problems {
		errs[i] = utils.ParamError{Key: p.Path, Code: string(httperr.CodeInvalidValue), Want: p.Want, Message: p.Message}
	}
	return errs
}

// parametersWriteResponse is a parameter write's answer: the resolved view
// it produced, and what it means for the models already running.
type parametersWriteResponse struct {
	resolvedResponse
	Runs *RunsReport `json:"runs,omitempty"`
}

// applyPatchToConfig mutates sc in-place per RFC 7396 over the 4-tier
// tree. Validation has already run against the pre-mutation cfg, so any
// error here is an unexpected env-value constraint failure.
func applyPatchToConfig(sc *pkgConfig.ServiceConfig, patch *paramPatchBody) error {
	if patch.Defaults != nil {
		if sc.Defaults == nil {
			sc.Defaults = &pkgConfig.AppDefaultsConfig{}
		}
		if err := applyLeafPatch(&sc.Defaults.Parameters, &sc.Defaults.Environment,
			&specPatch{Parameters: patch.Defaults.Parameters, Environment: patch.Defaults.Environment}); err != nil {
			return err
		}
		if len(patch.Defaults.Install) > 0 {
			var err error
			sc.Defaults.Install, err = pkgConfig.MergeInstallPatch(sc.Defaults.Install, patch.Defaults.Install)
			if err != nil {
				return err
			}
		}
		if err := applyEndpointsPatch(&sc.Defaults.Endpoints, patch.Defaults.Endpoints); err != nil {
			return err
		}
	}
	for m, mp := range patch.Models {
		if mp == nil {
			// RFC 7396: null on the subtree deletes it outright.
			delete(sc.Models, m)
			continue
		}
		spec := pkgConfig.EnsureModelSpecFor(sc, m)
		if err := applyLeafPatch(&spec.Parameters, &spec.Environment, &mp.specPatch); err != nil {
			return err
		}
		if err := applyEndpointsPatch(&spec.Endpoints, mp.Endpoints); err != nil {
			return err
		}
		if err := applyModelCellPatch(&spec, mp); err != nil {
			return err
		}
		sc.Models[m] = spec
		pkgConfig.CleanupModelSpecFor(sc, m)
	}
	for n, np := range patch.Nodes {
		if np == nil {
			delete(sc.Nodes, n)
			continue
		}
		nspec := pkgConfig.EnsureNodeSpecFor(sc, n)
		if len(np.Install) > 0 {
			var err error
			nspec.Install, err = pkgConfig.MergeInstallPatch(nspec.Install, np.Install)
			if err != nil {
				return err
			}
		}
		leafSpec := &specPatch{Parameters: np.Parameters, Environment: np.Environment}
		if err := applyLeafPatch(&nspec.Parameters, &nspec.Environment, leafSpec); err != nil {
			return err
		}
		if err := applyEndpointsPatch(&nspec.Endpoints, np.Endpoints); err != nil {
			return err
		}
		for m, mp := range np.Models {
			if mp == nil {
				if nspec.Models != nil {
					delete(nspec.Models, m)
				}
				continue
			}
			if nspec.Models == nil {
				nspec.Models = make(map[string]pkgConfig.NodeModelSpec)
			}
			nm := nspec.Models[m]
			params := nm.Parameters
			env := nm.Environment
			if err := applyLeafPatch(&params, &env, mp); err != nil {
				return err
			}
			endpoints := nm.Endpoints
			if err := applyEndpointsPatch(&endpoints, mp.Endpoints); err != nil {
				return err
			}
			nm.Parameters = params
			nm.Environment = env
			nm.Endpoints = endpoints
			nspec.Models[m] = nm
		}
		sc.Nodes[n] = nspec
		for m, mp := range np.Models {
			if mp != nil {
				pkgConfig.CleanupNodeModelSpecFor(sc, n, m)
			}
		}
		pkgConfig.CleanupNodeSpecFor(sc, n)
	}
	return nil
}

// applyModelCellPatch applies from and request, which only a model cell
// holds. Validation has already run.
func applyModelCellPatch(spec *pkgConfig.ModelSpec, mp *modelPatch) error {
	switch {
	case len(mp.From) == 0:
	case isJSONNull(mp.From):
		spec.From = ""
	default:
		if err := json.Unmarshal(mp.From, &spec.From); err != nil {
			return fmt.Errorf("decode from: %w", err)
		}
	}
	switch {
	case len(mp.Request) == 0:
	case isJSONNull(mp.Request):
		spec.Request = nil
	default:
		var patch map[string]any
		if err := json.Unmarshal(mp.Request, &patch); err != nil {
			return fmt.Errorf("decode request: %w", err)
		}
		spec.Request = mergePatchObject(spec.Request, patch)
		if len(spec.Request) == 0 {
			spec.Request = nil
		}
	}
	return nil
}

// mergePatchObject applies an RFC 7396 merge patch to an object: null
// deletes a member, an object merges into an object member, anything
// else replaces it. dst is modified and returned, allocated when nil.
func mergePatchObject(dst, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			cur, _ := dst[k].(map[string]any)
			dst[k] = mergePatchObject(cur, pv)
		default:
			dst[k] = v
		}
	}
	return dst
}

// applyEndpointsPatch merges per-endpoint overlay patches into the tier's
// endpoints map. RFC 7396: a nil endpointPatch at endpoints.<name>
// deletes the whole overlay for that endpoint; a non-nil patch merges
// its parameters/environment into the existing overlay (creating if
// absent). Empty resulting overlay (no params + no env after merge) is
// dropped to keep the on-disk YAML clean.
func applyEndpointsPatch(dst *map[string]pkgConfig.EndpointOverlay, eps map[string]*endpointPatch) error {
	if len(eps) == 0 {
		return nil
	}
	for name, ep := range eps {
		if ep == nil {
			if *dst != nil {
				delete(*dst, name)
			}
			continue
		}
		existing := (*dst)[name]
		params := existing.Parameters
		env := existing.Environment
		leaf := &specPatch{Parameters: ep.Parameters, Environment: ep.Environment}
		if err := applyLeafPatch(&params, &env, leaf); err != nil {
			return err
		}
		if len(params) == 0 && len(env) == 0 {
			if *dst != nil {
				delete(*dst, name)
			}
			continue
		}
		if *dst == nil {
			*dst = make(map[string]pkgConfig.EndpointOverlay)
		}
		(*dst)[name] = pkgConfig.EndpointOverlay{Parameters: params, Environment: env}
	}
	return nil
}

// applyLeafPatch sets / deletes keys on the two leaf maps per the
// RFC 7396 rule: null deletes, other values set. Maps are lazily
// allocated so absent-at-both-tiers null is a clean no-op.
func applyLeafPatch(params, env *map[string]string, p *specPatch) error {
	if p == nil {
		return nil
	}
	applyOne := func(dst *map[string]string, kv map[string]json.RawMessage, isEnv bool) error {
		for k, raw := range kv {
			if isJSONNull(raw) {
				if *dst != nil {
					delete(*dst, k)
				}
				continue
			}
			s, err := rawToString(raw)
			if err != nil {
				return fmt.Errorf("coerce %q: %w", k, err)
			}
			_ = isEnv // env validation ran in validatePatch (H4 — pre-apply).
			if *dst == nil {
				*dst = make(map[string]string)
			}
			(*dst)[k] = s
		}
		return nil
	}
	if err := applyOne(params, p.Parameters, false); err != nil {
		return err
	}
	return applyOne(env, p.Environment, true)
}

// ============================================================================
// GET shims — thin Resolve-backed compat for callers predating /resolved
// ============================================================================

// HandleInternalGetAppParameters handles GET /providers/:name/parameters.
// Thin shim over Resolve for backward compatibility with callers that
// predate /resolved; defaults-only view (no node/model).
func (e *ParamsExecutor) HandleInternalGetAppParameters(c *gin.Context) {
	provider := c.Param("name")
	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	// App-level response: defaults only. node="" + model="" gives Tier-0 only.
	resolved := cfg.Resolve("", "")
	c.JSON(http.StatusOK, buildLegacyAppResponse(provider, resolved))
}

// HandleInternalGetNodeParameters handles GET /providers/:name/nodes/parameters?node=X.
func (e *ParamsExecutor) HandleInternalGetNodeParameters(c *gin.Context) {
	provider := c.Param("name")
	node := c.Query("node")
	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	resolved := cfg.Resolve(node, "")
	c.JSON(http.StatusOK, buildLegacyNodeResponse(provider, node, resolved))
}

// HandleInternalGetModelParameters handles GET /providers/:name/models/parameters?model=X.
func (e *ParamsExecutor) HandleInternalGetModelParameters(c *gin.Context) {
	provider := c.Param("name")
	model := c.Query("model")
	cfg, err := getAppConfig(e.appsConfig(), provider)
	if err != nil {
		NotFound(c, err.Error())
		return
	}
	resolved := cfg.Resolve(e.nodename(), model)
	c.JSON(http.StatusOK, buildLegacyModelResponse(provider, model, resolved))
}

// ============================================================================
// POST /providers/:name/resolve — auto-sentinel filter (worker + coord)
// ============================================================================

type resolveRequest struct {
	Model      string            `json:"model"`
	Parameters map[string]string `json:"parameters"`
}

type resolveResponse struct {
	Resolved map[string]string `json:"resolved"`
}

// HandleInternalResolve handles POST /zzrouter/internal/providers/:name/resolve.
// Filters out "auto" values and returns the remaining parameters.
// Registered on ALL nodes so coordinators can route here per-worker.
func (e *ParamsExecutor) HandleInternalResolve(c *gin.Context) {
	var req resolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, "invalid request: "+err.Error())
		return
	}
	for k, v := range req.Parameters {
		if err := validateEnvValue(k, v); err != nil {
			BadRequest(c, err.Error())
			return
		}
	}
	c.JSON(http.StatusOK, &resolveResponse{Resolved: pkgConfig.FilterAutoValues(req.Parameters)})
}

// RetiredHandler returns 410 Gone for the parameter mutators that
// PATCH .../parameters replaced.
//
// It answers in the Problem envelope every other management failure uses
// (including the other 410 on this API, expired cluster pairing) rather
// than the parameter-validation body it used to emit. A caller branching
// on the documented top-level `code` found nothing there, which is the
// one thing an error shape has to get right.
func (e *ParamsExecutor) RetiredHandler(c *gin.Context) {
	_ = e // unused — method for symmetry with other handlers
	problem := utils.NewProblemDetails(http.StatusGone, "Gone",
		"this route is retired; PATCH /zzrouter/v1/providers/{name}/parameters "+
			"with Content-Type: application/merge-patch+json is the single mutator",
		c.Request.URL.Path)
	problem.Code = string(httperr.CodeRetired)
	if reqID, exists := c.Get("request_id"); exists {
		if id, ok := reqID.(string); ok {
			problem.RequestID = id
		}
	}
	c.Header("Content-Type", "application/problem+json")
	c.JSON(http.StatusGone, problem)
}

func (e *ParamsExecutor) listRunningModelNames(ctx context.Context, provider string) []string {
	if e.protocol == nil {
		return nil
	}
	providerImpl, ok := e.protocol(provider)
	if !ok {
		return nil
	}
	resolved, ok := e.backend.Resolve(provider)
	if !ok {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := providerImpl.ListRunningModels(tctx, protocol.TargetOf(resolved))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names
}

// checkHealth probes the provider's own endpoint, sending the credential
// it declares.
func (e *ParamsExecutor) checkHealth(ctx context.Context, cfg *pkgConfig.ServiceConfig) bool {
	target, ok := backend.Target(cfg)
	if !ok {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := target.NewRequest(checkCtx, http.MethodGet, "", nil)
	if err != nil {
		return false
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (e *ParamsExecutor) waitForHealth(ctx context.Context, cfg *pkgConfig.ServiceConfig, timeout time.Duration) bool {
	if _, ok := backend.Target(cfg); !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for ctx.Err() == nil {
		if e.checkHealth(ctx, cfg) {
			return true
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}
