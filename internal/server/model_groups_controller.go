package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/model/autoroute"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stperic/zzrouter/pkg/utils"
)

// paramErrorCarrier wraps a utils.ParamError so it can flow back
// through an `error` return without losing structure. Used by closures
// passed to GroupStore.ApplyPatch.
type paramErrorCarrier struct{ inner utils.ParamError }

func (e *paramErrorCarrier) Error() string { return e.inner.Message }

// respondMutatorError maps store-level errors from ApplyPatch /
// SetReplica / PatchReplica / DeleteReplica into the closed-enum HTTP
// envelope used by the routes agent-control surface. Returns true iff
// err was non-nil (so the caller can short-circuit).
func respondMutatorError(c *gin.Context, name, replica string, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, modelgroup.ErrGroupNotFound):
		RespondWithParamErrors(c, http.StatusNotFound, "Not Found", []utils.ParamError{{
			Code:    string(httperr.CodeRouteUnknown),
			Message: "model group " + name + " not found",
		}})
	case errors.Is(err, modelgroup.ErrReplicaNotFound):
		RespondWithParamErrors(c, http.StatusNotFound, "Not Found", []utils.ParamError{{
			Code:    string(httperr.CodeReplicaUnknown),
			Message: "replica " + replica + " not found in group " + name,
		}})
	case errors.Is(err, modelgroup.ErrReplicaLastInGroup):
		RespondWithParamErrors(c, http.StatusConflict, "Conflict", []utils.ParamError{{
			Code:    string(httperr.CodeReplicaLastInGroup),
			Message: "cannot delete the last replica in group " + name,
		}})
	case errors.Is(err, modelgroup.ErrRouteNotClaimed):
		RespondWithParamErrors(c, http.StatusConflict, "Conflict", []utils.ParamError{{
			Code: string(httperr.CodeRouteNotClaimed),
			Message: "model group " + name + " is not a claimed auto-route; " +
				"only a route the generator created and an agent claimed has an owner to release",
		}})
	default:
		var pec *paramErrorCarrier
		if errors.As(err, &pec) {
			RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{pec.inner})
			return true
		}
		BadRequest(c, err.Error())
	}
	return true
}

// requirePrincipal returns the caller principal or writes a 401 + false
// when the request is anonymous. All group mutators (PUT/PATCH/DELETE)
// go through this so the implicit-claim path never stamps an empty
// AutoOwner — that would make a claimed group indistinguishable from a
// hand-rolled greenfield route.
func requirePrincipal(c *gin.Context) (string, bool) {
	p := PrincipalFromContext(c)
	if p == "" {
		Unauthorized(c, "Mutator routes require an authenticated principal")
		c.Abort()
		return "", false
	}
	return p, true
}

// ttlSecondsMin / ttlSecondsMax bound the ephemeral-route TTL range.
// 60s minimum protects against churn (the reaper ticks at 1 min);
// 86400s maximum (24h) keeps ephemeral routes from accidentally
// becoming long-lived shadow state — agents that need longer should
// drop the TTL and audit via Owner.
const (
	ttlSecondsMin = 60
	ttlSecondsMax = 86400
	ownerMaxLen   = 128
)

// validateTTLAndOwner enforces the bounds documented on modelGroupRequest.
// Returns a typed ParamError so the controller can wrap it into the
// closed-enum envelope. Empty owner is allowed at any TTL; the audit
// surface comes from the authenticated principal, not this free-text
// field.
func validateTTLAndOwner(ttl int, owner string) *utils.ParamError {
	if ttl != 0 {
		if ttl < ttlSecondsMin || ttl > ttlSecondsMax {
			return &utils.ParamError{
				Key:     "ttl_seconds",
				Code:    string(httperr.CodeOutOfRange),
				Got:     ttl,
				Min:     ttlSecondsMin,
				Max:     ttlSecondsMax,
				Message: "ttl_seconds must be between 60 and 86400",
			}
		}
	}
	if len(owner) > ownerMaxLen {
		return &utils.ParamError{
			Key:     "owner",
			Code:    string(httperr.CodeOutOfRange),
			Got:     len(owner),
			Max:     ownerMaxLen,
			Message: "owner must be at most 128 characters",
		}
	}
	return nil
}

// setExpiresAtHeader echoes the ephemeral-route expiry on responses
// that emit a single route. Empty (zero ExpiresAt) emits no header so
// perpetual routes don't carry a misleading absent-value signal.
func setExpiresAtHeader(c *gin.Context, g modelgroup.ModelGroup) {
	if g.ExpiresAt.IsZero() {
		return
	}
	c.Header("X-zzrouter-Expires-At", g.ExpiresAt.UTC().Format(time.RFC3339))
}

// claimIfAuto applies the implicit-claim rule: any user mutation on an
// auto-managed group flips AutoManaged=false and stamps AutoOwner with
// the caller principal. Pre-state is read from `pre`; the next state
// (`next`) is returned with the claim fields rewritten if pre was
// auto-managed. Runs inside ApplyPatch's lock so two parallel mutations
// serialize and only the first wins the AutoOwner slot.
func claimIfAuto(pre, next modelgroup.ModelGroup, principal string) modelgroup.ModelGroup {
	if pre.AutoManaged {
		next.AutoManaged = false
		next.AutoOwner = principal
	}
	return next
}

// readMergePatchBody enforces Content-Type and reads the raw JSON body
// for a merge-patch request. Returns nil + writes a 400 on any failure.
func readMergePatchBody(c *gin.Context) json.RawMessage {
	if ct := c.GetHeader("Content-Type"); ct != "" && ct != "application/merge-patch+json" && ct != "application/json" {
		BadRequest(c, "Content-Type must be application/merge-patch+json")
		return nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		BadRequest(c, "read body: "+err.Error())
		return nil
	}
	if !json.Valid(raw) {
		BadRequest(c, "merge-patch body is not valid JSON")
		return nil
	}
	return raw
}

// ModelGroupsController handles HTTP requests for model group operations.
//
// idemStore is a dedicated in-memory Idempotency-Key cache for the
// model-groups mutator surface. Sharing the type but not the instance
// with /runs and other surfaces keeps key spaces isolated.
//
// loadTracker, latencyTracker, healthChecker are optional. Tests and
// minimal-router setups pass nil; the /state handler omits the
// corresponding fields when the source is absent.
type ModelGroupsController struct {
	groupStore     *modelgroup.GroupStore
	cooldowns      *fallback.CooldownManager
	loadTracker    *fallback.LoadTracker
	latencyTracker *fallback.LatencyTracker
	healthChecker  *fallback.HealthChecker
	logStore       *inferencelog.Store
	routeEvents    *route_events.Bus
	pricing        *pricing.Store
	routePrefix    string
	idemStore      *idempotencyStore
}

// NewModelGroupsController creates a new model groups controller. The
// tracker pointers feed the read-side /state endpoint; the log store
// backs /history. Passing nil for any of them is supported — the
// corresponding endpoints degrade (state omits fields, history 503).
func NewModelGroupsController(
	groupStore *modelgroup.GroupStore,
	cooldowns *fallback.CooldownManager,
	loadTracker *fallback.LoadTracker,
	latencyTracker *fallback.LatencyTracker,
	healthChecker *fallback.HealthChecker,
	logStore *inferencelog.Store,
	routeEvents *route_events.Bus,
	pricingStore *pricing.Store,
	routePrefix string,
) *ModelGroupsController {
	return &ModelGroupsController{
		groupStore:     groupStore,
		cooldowns:      cooldowns,
		loadTracker:    loadTracker,
		latencyTracker: latencyTracker,
		healthChecker:  healthChecker,
		logStore:       logStore,
		routeEvents:    routeEvents,
		pricing:        pricingStore,
		routePrefix:    routePrefix,
		idemStore:      newIdempotencyStore(),
	}
}

// RegisterPublicRoutes registers model group routes on the public API.
// All state-mutating routes accept an Idempotency-Key header (24h
// replay window); retries with the same key + same body return the
// recorded response without re-executing.
func (ctrl *ModelGroupsController) RegisterPublicRoutes(router *gin.RouterGroup) {
	idem := ctrl.idemStore.middleware()
	router.GET("/model-groups", ctrl.ListModelGroups)
	router.GET("/model-groups/schema", ctrl.GetRoutesSchema)
	router.GET("/model-groups/events", ctrl.handleRouteEventsStream)
	router.GET("/model-groups/:name", ctrl.GetModelGroup)
	router.GET("/model-groups/:name/state", ctrl.GetModelGroupState)
	router.GET("/model-groups/:name/history", ctrl.GetModelGroupHistory)
	router.PUT("/model-groups/:name", idem, ctrl.CreateOrUpdateModelGroup)
	router.PATCH("/model-groups/:name", idem, ctrl.PatchModelGroup)
	router.DELETE("/model-groups/:name", idem, ctrl.DeleteModelGroup)
	router.PUT("/model-groups/:name/replicas/:replica", idem, ctrl.PutReplica)
	router.PATCH("/model-groups/:name/replicas/:replica", idem, ctrl.PatchReplica)
	router.DELETE("/model-groups/:name/replicas/:replica", idem, ctrl.DeleteReplica)
	router.DELETE("/model-groups/:name/owner", idem, ctrl.ReleaseModelGroupOwner)
	router.POST("/model-groups/preview", ctrl.PreviewRoutes)
	router.POST("/model-groups/:name/preview", ctrl.PreviewRoute)
	router.POST("/model-groups/reload", ctrl.ReloadModelGroups)
}

// healthCheckConfig is the API representation of health check settings (shared by request and response).
type healthCheckConfig struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// modelGroupResponse is the API representation of a model group.
type modelGroupResponse struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Strategy    string             `json:"strategy"`
	HealthCheck *healthCheckConfig `json:"health_check,omitempty"`
	Replicas    []replicaResponse  `json:"replicas"`
	Params      map[string]any     `json:"params,omitempty"`
	AutoManaged bool               `json:"auto_managed,omitempty"`
	AutoOwner   string             `json:"auto_owner,omitempty"`
	Owner       string             `json:"owner,omitempty"`
	ExpiresAt   *time.Time         `json:"expires_at,omitempty"`
}

// replicaResponse is the API representation of a replica within a group.
type replicaResponse struct {
	Name              string         `json:"name"`
	Model             string         `json:"model"`
	Provider          string         `json:"provider"`
	Node              string         `json:"node,omitempty"`
	Priority          int            `json:"priority"`
	Timeout           string         `json:"timeout,omitempty"`
	OnDemand          bool           `json:"on_demand,omitempty"`
	GPUMemoryRequired string         `json:"gpu_memory_required,omitempty"`
	MaxRetries        int            `json:"max_retries,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	Params            map[string]any `json:"params,omitempty"`
	CooldownSeconds   float64        `json:"cooldown_seconds,omitempty"`
	CooldownReason    string         `json:"cooldown_reason,omitempty"`
}

// ListModelGroups handles GET /zzrouter/v1/model-groups
func (ctrl *ModelGroupsController) ListModelGroups(c *gin.Context) {
	all := ctrl.groupStore.List()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]modelGroupResponse, 0, len(all))
	for _, name := range names {
		result = append(result, toModelGroupResponse(name, all[name], ctrl.cooldowns))
	}

	respondListWithMetadata(c, result, len(result), false, map[string]any{
		"route_prefix": ctrl.routePrefix,
	})
}

// GetModelGroup handles GET /zzrouter/v1/model-groups/:name.
//
// ETag is computed over the *configuration projection* (toModelGroupConfigProjection)
// — i.e. omitting live derived fields like cooldown_seconds/cooldown_reason.
// Hashing the live response body would flicker the ETag on every poll
// even with no underlying mutation, defeating the cache contract.
// If-None-Match against the stored config-hash short-circuits with 304.
func (ctrl *ModelGroupsController) GetModelGroup(c *gin.Context) {
	name := c.Param("name")

	group := ctrl.groupStore.Get(name)
	if group == nil {
		NotFound(c, "model group not found")
		return
	}

	cfgProj := toModelGroupConfigProjection(name, *group)
	cfgBytes, err := json.Marshal(cfgProj)
	if err == nil {
		sum := sha256.Sum256(cfgBytes)
		etag := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
		if c.GetHeader("If-None-Match") == etag {
			c.Header("ETag", etag)
			c.Status(304)
			return
		}
		c.Header("ETag", etag)
	}

	setExpiresAtHeader(c, *group)
	respondSuccess(c, "Model group retrieved successfully", toModelGroupResponse(name, *group, ctrl.cooldowns))
}

// toModelGroupConfigProjection returns the *static* shape of a model
// group — every field except the live derived cooldown values — so it
// can be SHA256-hashed for ETag. Replicas are sorted by priority in
// the store; the response inherits that order deterministically.
func toModelGroupConfigProjection(name string, group modelgroup.ModelGroup) modelGroupResponse {
	reps := make([]replicaResponse, len(group.Replicas))
	for i, r := range group.Replicas {
		reps[i] = replicaResponse{
			Name:              r.Name,
			Model:             r.Model,
			Provider:          r.App,
			Node:              r.Node,
			Priority:          r.Priority,
			OnDemand:          r.OnDemand,
			GPUMemoryRequired: r.GPUMemoryRequired,
			MaxRetries:        r.MaxRetries,
			Tags:              r.Tags,
			Params:            r.Params,
		}
		if r.Timeout.Duration > 0 {
			reps[i].Timeout = r.Timeout.Duration.String()
		}
	}
	resp := modelGroupResponse{
		Name:        name,
		Description: group.Description,
		Strategy:    string(group.Strategy),
		Replicas:    reps,
		Params:      group.Params,
		AutoManaged: group.AutoManaged,
		AutoOwner:   group.AutoOwner,
		Owner:       group.Owner,
	}
	if !group.ExpiresAt.IsZero() {
		t := group.ExpiresAt.UTC().Truncate(time.Second)
		resp.ExpiresAt = &t
	}
	if group.HealthCheck != nil {
		hc := &healthCheckConfig{Path: group.HealthCheck.Path}
		if hc.Path == "" {
			hc.Path = "/health"
		}
		if group.HealthCheck.Interval.Duration > 0 {
			hc.Interval = group.HealthCheck.Interval.Duration.String()
		} else {
			hc.Interval = "30s"
		}
		if group.HealthCheck.Timeout.Duration > 0 {
			hc.Timeout = group.HealthCheck.Timeout.Duration.String()
		} else {
			hc.Timeout = "5s"
		}
		resp.HealthCheck = hc
	}
	return resp
}

// modelGroupRequest is the API input for creating/updating a model group.
type modelGroupRequest struct {
	Description string             `json:"description,omitempty"`
	Strategy    string             `json:"strategy,omitempty" binding:"omitempty,oneof=priority least-load fastest"`
	HealthCheck *healthCheckConfig `json:"health_check,omitempty"`
	Replicas    []replicaRequest   `json:"replicas" binding:"dive"`
	Params      map[string]any     `json:"params,omitempty"`
	// TTLSeconds creates an ephemeral route: ExpiresAt = now + ttl.
	// Range [60, 86400]; 0 means perpetual. The reaper deletes the
	// route shortly after ExpiresAt (1-minute granularity).
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Owner is a free-text audit tag echoed back on GET. Max 128
	// chars. Informational only — no auth coupling.
	Owner string `json:"owner,omitempty"`
}

// replicaRequest is the API input for a replica within a group.
type replicaRequest struct {
	Name              string         `json:"name" binding:"required"`
	Model             string         `json:"model" binding:"required"`
	Provider          string         `json:"provider" binding:"required"`
	Node              string         `json:"node,omitempty"`
	Priority          int            `json:"priority,omitempty"`
	Timeout           string         `json:"timeout,omitempty"`
	OnDemand          bool           `json:"on_demand,omitempty"`
	GPUMemoryRequired string         `json:"gpu_memory_required,omitempty"`
	MaxRetries        int            `json:"max_retries,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	Params            map[string]any `json:"params,omitempty"`
}

// CreateOrUpdateModelGroup handles PUT /zzrouter/v1/model-groups/:name
func (ctrl *ModelGroupsController) CreateOrUpdateModelGroup(c *gin.Context) {
	name := c.Param("name")
	principal, ok := requirePrincipal(c)
	if !ok {
		return
	}

	var req modelGroupRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	// Reserved names apply to creation only: a group that already exists
	// (e.g. one predating this check, or an auto-route claimed under an
	// empty route_prefix) must stay updatable, or a later route addition
	// would strand it.
	existing := ctrl.groupStore.Get(name)
	if reservedModelGroupNames[name] && existing == nil {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{
			Key:     "name",
			Code:    string(httperr.CodeReservedName),
			Got:     name,
			Want:    "a name other than: " + reservedNameList(reservedModelGroupNames),
			Message: reservedNameMessage("model group name", name, "/zzrouter/v1/model-groups/", reservedModelGroupNames),
		}})
		return
	}
	if perr := validateTTLAndOwner(req.TTLSeconds, req.Owner); perr != nil {
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{*perr})
		return
	}

	group := modelgroup.ModelGroup{
		Description: req.Description,
		Strategy:    modelgroup.StrategyType(req.Strategy),
		Params:      req.Params,
		Owner:       req.Owner,
	}
	if req.TTLSeconds > 0 {
		group.ExpiresAt = utils.NowUTC().Add(time.Duration(req.TTLSeconds) * time.Second)
	}
	if req.HealthCheck != nil {
		hc := &modelgroup.HealthCheckConfig{}
		hc.Path = req.HealthCheck.Path
		if req.HealthCheck.Interval != "" {
			parsed, err := time.ParseDuration(req.HealthCheck.Interval)
			if err != nil {
				BadRequest(c, "Invalid health_check interval: "+err.Error())
				return
			}
			hc.Interval = modelgroup.Duration{Duration: parsed}
		}
		if req.HealthCheck.Timeout != "" {
			parsed, err := time.ParseDuration(req.HealthCheck.Timeout)
			if err != nil {
				BadRequest(c, "Invalid health_check timeout: "+err.Error())
				return
			}
			hc.Timeout = modelgroup.Duration{Duration: parsed}
		}
		group.HealthCheck = hc
	}
	for _, d := range req.Replicas {
		rep := modelgroup.Replica{
			Name:              d.Name,
			Model:             d.Model,
			App:               d.Provider,
			Node:              d.Node,
			Priority:          d.Priority,
			OnDemand:          d.OnDemand,
			GPUMemoryRequired: d.GPUMemoryRequired,
			MaxRetries:        d.MaxRetries,
			Tags:              d.Tags,
			Params:            d.Params,
		}
		if d.Timeout != "" {
			parsed, err := time.ParseDuration(d.Timeout)
			if err != nil {
				BadRequest(c, "Invalid timeout for replica "+d.Name+": "+err.Error())
				return
			}
			rep.Timeout = modelgroup.Duration{Duration: parsed}
		}
		group.Replicas = append(group.Replicas, rep)
	}

	// Implicit claim on PUT-replace: if the existing entry is auto-
	// managed, the new entry inherits AutoManaged=false + caller as
	// AutoOwner. Last-writer-wins matches PUT semantics; concurrent
	// PUTs both observing AutoManaged=true is bounded by the same
	// last-write contract.
	if existing != nil && existing.AutoManaged {
		group.AutoManaged = false
		group.AutoOwner = principal
	}

	if err := ctrl.groupStore.Set(name, group); err != nil {
		BadRequest(c, err.Error())
		return
	}

	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Group saved in memory but failed to persist: "+err.Error())
		return
	}

	setExpiresAtHeader(c, group)
	respondSuccess(c, "Model group saved successfully", toModelGroupResponse(name, group, ctrl.cooldowns))
}

// PatchModelGroup handles PATCH /zzrouter/v1/model-groups/:name (RFC 7396).
// Three-meaning rule: absent key = inherit, null = clear, value = set.
// Auto-managed groups are claimed by any mutation: AutoManaged flips
// false and AutoOwner is stamped with the caller principal. DELETE
// /:name/owner is the single path back to autoroute generation.
func (ctrl *ModelGroupsController) PatchModelGroup(c *gin.Context) {
	name := c.Param("name")
	principal, ok := requirePrincipal(c)
	if !ok {
		return
	}
	raw := readMergePatchBody(c)
	if raw == nil {
		return
	}

	var patched modelgroup.ModelGroup
	err := ctrl.groupStore.ApplyPatch(name, func(current modelgroup.ModelGroup) (modelgroup.ModelGroup, error) {
		next, perr := applyModelGroupPatch(current, raw)
		if perr != nil {
			return modelgroup.ModelGroup{}, &paramErrorCarrier{inner: *perr}
		}
		next = claimIfAuto(current, next, principal)
		patched = next
		return next, nil
	})
	if respondMutatorError(c, name, "", err) {
		return
	}
	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Group patched in memory but failed to persist: "+err.Error())
		return
	}
	setExpiresAtHeader(c, patched)
	respondSuccess(c, "Model group patched successfully", toModelGroupResponse(name, patched, ctrl.cooldowns))
}

// replicaUpsertRequest is the PUT body shape. URL :replica is the
// canonical name, so the body has no name field. Model and provider
// remain required (PUT supplies the full replica).
type replicaUpsertRequest struct {
	Model             string         `json:"model" binding:"required"`
	Provider          string         `json:"provider" binding:"required"`
	Node              string         `json:"node,omitempty"`
	Priority          int            `json:"priority,omitempty"`
	Timeout           string         `json:"timeout,omitempty"`
	OnDemand          bool           `json:"on_demand,omitempty"`
	GPUMemoryRequired string         `json:"gpu_memory_required,omitempty"`
	MaxRetries        int            `json:"max_retries,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	Params            map[string]any `json:"params,omitempty"`
}

// PutReplica handles PUT /zzrouter/v1/model-groups/:name/replicas/:replica.
// Upsert: replaces the named replica or adds it. URL identity wins —
// the body carries no name field.
func (ctrl *ModelGroupsController) PutReplica(c *gin.Context) {
	name := c.Param("name")
	replicaName := c.Param("replica")
	principal, ok := requirePrincipal(c)
	if !ok {
		return
	}

	var req replicaUpsertRequest
	if !BindJSONStrict(c, &req) {
		return
	}

	rep := modelgroup.Replica{
		Name:              replicaName,
		Model:             req.Model,
		App:               req.Provider,
		Node:              req.Node,
		Priority:          req.Priority,
		OnDemand:          req.OnDemand,
		GPUMemoryRequired: req.GPUMemoryRequired,
		MaxRetries:        req.MaxRetries,
		Tags:              req.Tags,
		Params:            req.Params,
	}
	if req.Timeout != "" {
		parsed, err := time.ParseDuration(req.Timeout)
		if err != nil {
			BadRequest(c, "Invalid timeout: "+err.Error())
			return
		}
		rep.Timeout = modelgroup.Duration{Duration: parsed}
	}

	var snapshot modelgroup.ModelGroup
	err := ctrl.groupStore.ApplyPatch(name, func(current modelgroup.ModelGroup) (modelgroup.ModelGroup, error) {
		pre := current
		found := false
		for i := range current.Replicas {
			if current.Replicas[i].Name == replicaName {
				current.Replicas[i] = rep
				found = true
				break
			}
		}
		if !found {
			current.Replicas = append(current.Replicas, rep)
		}
		current = claimIfAuto(pre, current, principal)
		snapshot = current
		return current, nil
	})
	if respondMutatorError(c, name, replicaName, err) {
		return
	}
	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Replica saved in memory but failed to persist: "+err.Error())
		return
	}
	setExpiresAtHeader(c, snapshot)
	respondSuccess(c, "Replica saved successfully", toModelGroupResponse(name, snapshot, ctrl.cooldowns))
}

// PatchReplica handles PATCH /zzrouter/v1/model-groups/:name/replicas/:replica.
// Merge-patch on a single replica. Name is fixed by the URL.
func (ctrl *ModelGroupsController) PatchReplica(c *gin.Context) {
	name := c.Param("name")
	replicaName := c.Param("replica")
	principal, ok := requirePrincipal(c)
	if !ok {
		return
	}
	raw := readMergePatchBody(c)
	if raw == nil {
		return
	}

	var snapshot modelgroup.ModelGroup
	err := ctrl.groupStore.ApplyPatch(name, func(current modelgroup.ModelGroup) (modelgroup.ModelGroup, error) {
		pre := current
		for i := range current.Replicas {
			if current.Replicas[i].Name == replicaName {
				next, perr := applyReplicaPatch(current.Replicas[i], raw)
				if perr != nil {
					return current, &paramErrorCarrier{inner: *perr}
				}
				next.Name = replicaName
				current.Replicas[i] = next
				current = claimIfAuto(pre, current, principal)
				snapshot = current
				return current, nil
			}
		}
		return current, fmt.Errorf("%w: %q", modelgroup.ErrReplicaNotFound, replicaName)
	})
	if respondMutatorError(c, name, replicaName, err) {
		return
	}
	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Replica patched in memory but failed to persist: "+err.Error())
		return
	}
	setExpiresAtHeader(c, snapshot)
	respondSuccess(c, "Replica patched successfully", toModelGroupResponse(name, snapshot, ctrl.cooldowns))
}

// DeleteReplica handles DELETE /zzrouter/v1/model-groups/:name/replicas/:replica.
// Refuses to remove the last replica (409 replica_last_in_group).
func (ctrl *ModelGroupsController) DeleteReplica(c *gin.Context) {
	name := c.Param("name")
	replicaName := c.Param("replica")
	principal, ok := requirePrincipal(c)
	if !ok {
		return
	}

	err := ctrl.groupStore.ApplyPatch(name, func(current modelgroup.ModelGroup) (modelgroup.ModelGroup, error) {
		pre := current
		idx := -1
		for i := range current.Replicas {
			if current.Replicas[i].Name == replicaName {
				idx = i
				break
			}
		}
		if idx < 0 {
			return current, fmt.Errorf("%w: %q", modelgroup.ErrReplicaNotFound, replicaName)
		}
		if len(current.Replicas) <= 1 {
			return current, fmt.Errorf("%w: %q", modelgroup.ErrReplicaLastInGroup, replicaName)
		}
		current.Replicas = append(current.Replicas[:idx], current.Replicas[idx+1:]...)
		current = claimIfAuto(pre, current, principal)
		return current, nil
	})
	if respondMutatorError(c, name, replicaName, err) {
		return
	}
	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Replica deleted in memory but failed to persist: "+err.Error())
		return
	}
	respondSuccess(c, "Replica deleted successfully", nil)
}

// ReleaseModelGroupOwner handles
// DELETE /zzrouter/v1/model-groups/:name/owner — the single path back
// to autoroute generation. Clears AutoOwner and flips AutoManaged=true
// so the autoroute manager will reconcile this group again on the next
// cache refresh.
//
// Only a claimed auto-route can be released. AutoOwner is the one marker
// that distinguishes "generator built this, then an agent claimed it"
// (claimIfAuto sets AutoManaged=false and stamps the principal) from a
// hand-authored group, which carries no owner and so has nothing to
// release. Flipping an unowned group would hand the generator a name it
// never created: SyncAllFromCache would reap it as "model gone" and
// persist the deletion, and DeleteModelGroup would meanwhile refuse to
// remove it as an auto-route. Releasing an unclaimed group is therefore
// a 409 rather than a silent trap.
func (ctrl *ModelGroupsController) ReleaseModelGroupOwner(c *gin.Context) {
	name := c.Param("name")
	if _, ok := requirePrincipal(c); !ok {
		return
	}

	var snapshot modelgroup.ModelGroup
	err := ctrl.groupStore.ApplyPatch(name, func(current modelgroup.ModelGroup) (modelgroup.ModelGroup, error) {
		if current.AutoOwner == "" {
			return current, modelgroup.ErrRouteNotClaimed
		}
		current.AutoManaged = true
		current.AutoOwner = ""
		snapshot = current
		return current, nil
	})
	if respondMutatorError(c, name, "", err) {
		return
	}
	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Owner cleared in memory but failed to persist: "+err.Error())
		return
	}
	respondSuccess(c, "Model group owner released; autoroute will reconcile", toModelGroupResponse(name, snapshot, ctrl.cooldowns))
}

// DeleteModelGroup handles DELETE /zzrouter/v1/model-groups/:name
func (ctrl *ModelGroupsController) DeleteModelGroup(c *gin.Context) {
	name := c.Param("name")

	// Check if this is an auto-managed route
	if group := ctrl.groupStore.Get(name); group != nil && autoroute.IsAutoGroup(group) {
		Conflict(c, "cannot delete auto-route: this route is auto-managed because the model exists on multiple nodes")
		return
	}

	if !ctrl.groupStore.Delete(name) {
		NotFound(c, "model group not found")
		return
	}

	if err := ctrl.groupStore.Save(); err != nil {
		InternalNodeError(c, "Group deleted in memory but failed to persist: "+err.Error())
		return
	}

	respondSuccess(c, "Model group deleted successfully", nil)
}

// ReloadModelGroups handles POST /zzrouter/v1/model-groups/reload
func (ctrl *ModelGroupsController) ReloadModelGroups(c *gin.Context) {
	path := ctrl.groupStore.Path()
	if path == "" {
		InternalNodeError(c, "No model groups file path configured")
		return
	}
	if err := ctrl.groupStore.LoadFromFile(path); err != nil {
		InternalNodeError(c, "Failed to reload model groups: "+err.Error())
		return
	}
	respondSuccess(c, "Model groups reloaded successfully", nil)
}

func toModelGroupResponse(name string, group modelgroup.ModelGroup, cooldowns *fallback.CooldownManager) modelGroupResponse {
	reps := make([]replicaResponse, len(group.Replicas))
	for i, r := range group.Replicas {
		reps[i] = replicaResponse{
			Name:              r.Name,
			Model:             r.Model,
			Provider:          r.App,
			Node:              r.Node,
			Priority:          r.Priority,
			OnDemand:          r.OnDemand,
			GPUMemoryRequired: r.GPUMemoryRequired,
			MaxRetries:        r.MaxRetries,
			Tags:              r.Tags,
			Params:            r.Params,
		}
		if r.Timeout.Duration > 0 {
			reps[i].Timeout = r.Timeout.Duration.String()
		}
		if cooldowns != nil {
			if remaining, reason := cooldowns.GetEntry(r.Name); remaining > 0 {
				reps[i].CooldownSeconds = remaining.Seconds()
				reps[i].CooldownReason = reason
			}
		}
	}

	resp := modelGroupResponse{
		Name:        name,
		Description: group.Description,
		Strategy:    string(group.Strategy),
		Replicas:    reps,
		Params:      group.Params,
		AutoManaged: group.AutoManaged,
		AutoOwner:   group.AutoOwner,
		Owner:       group.Owner,
	}
	if !group.ExpiresAt.IsZero() {
		t := group.ExpiresAt.UTC().Truncate(time.Second)
		resp.ExpiresAt = &t
	}

	if group.HealthCheck != nil {
		hc := &healthCheckConfig{
			Path: group.HealthCheck.Path,
		}
		if hc.Path == "" {
			hc.Path = "/health"
		}
		if group.HealthCheck.Interval.Duration > 0 {
			hc.Interval = group.HealthCheck.Interval.Duration.String()
		} else {
			hc.Interval = "30s"
		}
		if group.HealthCheck.Timeout.Duration > 0 {
			hc.Timeout = group.HealthCheck.Timeout.Duration.String()
		} else {
			hc.Timeout = "5s"
		}
		resp.HealthCheck = hc
	}

	return resp
}
