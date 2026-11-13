package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func routePolicyRig(t *testing.T) (*accessRig, string, *Server) {
	t.Helper()
	rig := newAccessRigWithHooks(t, ModelHooks{Identity: func(name string) string { return strings.Split(name, "@")[0] }})
	rig.groups.set("coding", "local", "other", "paid")
	rig.seedTeam(t, "team", &teams.Team{Name: "Engineering", RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}, AllowedModels: []string{"local"}, ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"vllm", "ollama"}}})
	raw := rig.seedKey(t, "client", "team", "member", &keys.VirtualKey{Name: "Editor", Role: "user"})
	return rig, raw, &Server{access: rig.access}
}

func TestRouteAccessExactRequestAndResolvedIdentity(t *testing.T) {
	rig, raw, s := routePolicyRig(t)
	for _, name := range []string{"local", "coding@worker", "vllm/coding", "", "other"} {
		t.Run(name, func(t *testing.T) {
			c, w := rig.makeRequest(raw)
			require.True(t, rig.access.Authenticate(c, RoleUser))
			prepared := false
			require.False(t, rig.access.Enforce(c, name, func() ([]pricing.Quote, bool) { prepared = true; return nil, true }))
			require.Equal(t, 403, w.Code)
			require.False(t, prepared)
		})
	}
	for _, target := range []*resolver.Resolved{
		{OriginalName: "coding", ModelName: "coding", Provider: "vllm", Node: "worker"},
		{OriginalName: "coding", GroupName: "different", Provider: "vllm"},
	} {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		require.True(t, rig.access.Authorize(c, "coding"))
		require.False(t, s.admitAccessTargets(c, target))
		require.Equal(t, 403, w.Code)
	}
	c, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	target := &resolver.Resolved{OriginalName: "coding", GroupName: "coding", Candidates: []fallback.Candidate{{Name: "paid", Model: "paid", App: "groq"}, {Name: "model-denied", Model: "other", App: "ollama"}, {Name: "local", Model: "local", App: "vllm"}}}
	require.True(t, s.admitAccessTargets(c, target))
	require.Len(t, target.Candidates, 1)
	require.Equal(t, "local", target.Candidates[0].Name)
	require.Empty(t, rig.enforcer.GetAllSpendStates())
}

func TestRouteAccessMissingExpiryNoneAndCachedIdentity(t *testing.T) {
	rig, raw, s := routePolicyRig(t)
	for _, state := range []string{"missing", "expired", "empty", "none"} {
		t.Run(state, func(t *testing.T) {
			rig.groups.set("coding", "local")
			switch state {
			case "missing":
				delete(rig.groups.groups, "coding")
			case "expired":
				rig.groups.groups["coding"].ExpiresAt = utils.NowUTC().Add(-time.Second)
			case "empty":
				rig.groups.set("coding")
			case "none":
				_, err := NewTeamsService(rig.teams, rig.keys, rig.access).UpdateTeam("team", &UpdateTeamRequest{RouteAccess: teams.RouteAccess{Mode: "none"}}, "admin")
				require.NoError(t, err)
			}
			c, _ := rig.makeRequest(raw)
			require.True(t, rig.access.Authenticate(c, RoleUser))
			require.False(t, rig.access.Authorize(c, "coding"))
			require.False(t, s.catalogAllowed(c, "coding", "vllm"))
			require.NotEmpty(t, opaqueForwardLimit(GetAccessContext(c)))
		})
	}
	require.True(t, rig.teams.HasGatedTeam())
}

func TestRouteAccessCatalogKeepsAliasAndCapabilitiesWithoutDirectCollision(t *testing.T) {
	rig, raw, s := routePolicyRig(t)
	store := modelgroup.NewGroupStore()
	require.NoError(t, store.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "local", Model: "local", App: "vllm", Node: "worker"}}}))
	models := []*cache.CachedModel{{Name: "coding", Model: "coding", Provider: "vllm", Node: "worker"}, {Name: "local", Model: "local", Provider: "vllm", Node: "worker"}}
	service := NewModelService(&mockModelCache{models: models}, nil, nil, nil, nil, nil)
	h := &OpenAIModelHandlers{appsConfig: noAppsConfig, localNode: func() string { return "coord" }, modelService: service, groups: store, access: s.catalogAllowed, adapter: NewOpenAIAdapter(func() *config.AppsConfig { return nil })}
	c, w := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	h.handleModels(c)
	require.Equal(t, 200, w.Code)
	var body struct {
		Data []OpenAIModelObject `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, "coding", body.Data[0].ID)
	require.NotContains(t, w.Body.String(), "coding@worker")
	require.Contains(t, h.catalogIndex(c, models, "coding"), "local@worker", "route metadata must remain available for permitted destinations")
	require.NotContains(t, h.catalogIndex(c, models, "coding"), "coding@worker")
}

func TestRouteAccessTeamAssignmentValidationAndRetainedMissingNames(t *testing.T) {
	rig, _, _ := routePolicyRig(t)
	svc := NewTeamsService(rig.teams, rig.keys, rig.access)
	_, err := svc.UpdateTeam("team", &UpdateTeamRequest{RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"unknown"}}}, "admin")
	require.Error(t, err)
	delete(rig.groups.groups, "coding")
	name := "Renamed"
	updated, err := svc.UpdateTeam("team", &UpdateTeamRequest{Name: &name}, "admin")
	require.NoError(t, err)
	require.Equal(t, []string{"coding"}, updated.RouteAccess.Routes)
	_, err = svc.UpdateTeam("team", &UpdateTeamRequest{RouteAccess: teams.RouteAccess{Mode: "direct"}}, "admin")
	require.NoError(t, err)
	require.False(t, rig.teams.Get("team").RouteAccess.Restricted())
}

func TestRoutePreviewUsesTeamPolicyBudgetAndProviderCooldownWithoutEffects(t *testing.T) {
	rig, _, s := routePolicyRig(t)
	team := *rig.teams.Get("team")
	team.AllowedModels = nil
	team.QuotaConfig = quota.QuotaConfig{SpendLimit: 1}
	require.NoError(t, rig.teams.Update("team", &team))
	group := modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "blocked", Model: "paid", App: "groq"}, {Name: "cooling", Model: "local", App: "ollama"}, {Name: "winner", Model: "local", App: "vllm"}}}
	store := modelgroup.NewGroupStore()
	require.NoError(t, store.Set("coding", group))
	cooldowns := fallback.NewCooldownManager()
	cooldowns.SetCooldown("ollama", time.Minute, "rate_limit")
	quote := func(provider, model string) pricing.Quote {
		return pricing.Quote{Provider: provider, Model: model, Rates: &pricing.ModelPricing{InputCostPerToken: .001}}
	}
	ctrl := NewModelGroupsController(ModelGroupsDependencies{Groups: store, Access: s.access, ProviderCooldowns: cooldowns, Quote: quote})
	r := gin.New()
	r.POST("/preview", ctrl.PreviewRoutes)
	before := rig.enforcer.GetAllSpendStates()
	w := postPreview(t, r, "/preview", `{"route":"coding","team_id":"team"}`)
	require.Equal(t, 200, w.Code)
	preview := decodePreview(t, w)
	require.Len(t, preview.Assignments, 1)
	require.Equal(t, "winner", preview.Assignments[0].Replica)
	require.Equal(t, modelGroupETag("coding", *store.Get("coding")), preview.Assignments[0].RouteVersion)
	require.Equal(t, []previewSkip{{Replica: "blocked", Reason: "provider_access_denied"}, {Replica: "cooling", Reason: "provider_cooldown"}}, preview.Assignments[0].Skipped)
	require.Equal(t, before, rig.enforcer.GetAllSpendStates())
	require.NotEmpty(t, preview.Unchecked)
	decision, reservations := rig.enforcer.Check([]quota.QuotaScope{{Scope: quota.ScopeTeam, EntityID: "team", Quotas: quota.QuotaConfig{SpendLimit: 1, DefaultMaxTokens: 100000}}}, []pricing.Quote{quote("vllm", "local")})
	require.True(t, decision.Allowed)
	defer rig.enforcer.CancelReservations(reservations)
	held := rig.enforcer.GetAllSpendStates()
	w = postPreview(t, r, "/preview", `{"route":"coding","team_id":"team"}`)
	require.Equal(t, "budget_exhausted", decodePreview(t, w).Assignments[0].DeniedReason)
	require.Equal(t, preview.Assignments[0].RouteVersion, decodePreview(t, w).Assignments[0].RouteVersion)
	group.Description = "Updated"
	require.NoError(t, store.Set("coding", group))
	changed := decodePreview(t, postPreview(t, r, "/preview", `{"route":"coding","team_id":"team"}`))
	require.NotEqual(t, preview.Assignments[0].RouteVersion, changed.Assignments[0].RouteVersion)
	require.Equal(t, modelGroupETag("coding", *store.Get("coding")), changed.Assignments[0].RouteVersion)
	require.Equal(t, held, rig.enforcer.GetAllSpendStates())
	w = postPreview(t, r, "/preview", `{"team_id":"missing"}`)
	require.Equal(t, 404, w.Code)
	w = postPreview(t, r, "/preview", `{"input_tokens":-1}`)
	require.Equal(t, 400, w.Code)
}

func TestRouteConditionalWritesPreserveConcurrentChanges(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	request := func(method, path, body, match, none string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", match)
		req.Header.Set("If-None-Match", none)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// The test router's fixture names are discovered through its list response.
	list := request(http.MethodGet, "/zzrouter/v1/model-groups", "", "", "")
	var body struct {
		Data []modelGroupResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &body))
	require.NotEmpty(t, body.Data)
	path := "/zzrouter/v1/model-groups/" + body.Data[0].Name
	initial := request(http.MethodGet, path, "", "", "")
	require.Equal(t, 200, initial.Code)
	etag := initial.Header().Get("ETag")
	require.NotEmpty(t, etag)
	require.Equal(t, 200, request(http.MethodPatch, path, `{"description":"new"}`, etag, "").Code)
	require.Equal(t, 412, request(http.MethodPatch, path, `{"description":"lost update"}`, etag, "").Code)
	require.Equal(t, 412, request(http.MethodDelete, path, "", etag, "").Code)
	require.Equal(t, 412, request(http.MethodPut, path, `{"strategy":"priority","replicas":[{"name":"r","provider":"ollama","model":"m"}]}`, "", "*").Code)
	require.Contains(t, request(http.MethodGet, path, "", "", "").Body.String(), `"description":"new"`)
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		require.Equal(t, 412, request(method, "/zzrouter/v1/model-groups/gone", `{"description":"new"}`, etag, "").Code)
	}
}

func TestRouteAccessDiscoveryRoundTripsAndRejectsPins(t *testing.T) {
	rig, raw, s := routePolicyRig(t)
	store := modelgroup.NewGroupStore()
	require.NoError(t, store.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "r", App: "ollama", Model: "local"}}}))
	models := []*cache.CachedModel{{Name: "coding", Model: "coding", Provider: "ollama"}, {Name: "local", Model: "local", Provider: "ollama"}}
	service := NewModelService(&mockModelCache{models: models}, nil, nil, nil, nil, nil)
	openai := &OpenAIModelHandlers{appsConfig: noAppsConfig, localNode: func() string { return "coord" }, modelService: service, groups: store, access: s.catalogAllowed, adapter: NewOpenAIAdapter(noAppsConfig)}
	ollama := &OllamaHandlers{modelService: service, groups: store, access: s.catalogAllowed, adapter: &OllamaAdapter{}}
	request := func(method, path, body string, handler gin.HandlerFunc, restricted bool) *httptest.ResponseRecorder {
		c, w := rig.makeRequest(raw)
		if restricted {
			require.True(t, rig.access.Authenticate(c, RoleUser))
		}
		c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "model", Value: strings.TrimPrefix(path, "/v1/models")}}
		handler(c)
		return w
	}
	tags := request("GET", "/api/tags", "", ollama.handleOllamaTags, true)
	require.Equal(t, 200, tags.Code)
	var list OllamaTagsResponse
	require.NoError(t, json.Unmarshal(tags.Body.Bytes(), &list))
	require.Len(t, list.Models, 1)
	require.Equal(t, "coding", list.Models[0].Name)
	show := request("POST", "/api/show", `{"model":"coding"}`, ollama.handleOllamaShow, true)
	require.Equal(t, 200, show.Code)
	require.Contains(t, show.Body.String(), `"group":"coding"`)
	require.NotContains(t, show.Body.String(), "template")
	info := request("GET", "/v1/model/info", "", openai.handleModelInfo, true)
	var data OpenAIModelInfoResponse
	require.NoError(t, json.Unmarshal(info.Body.Bytes(), &data))
	require.Len(t, data.Data, 1)
	require.Equal(t, "coding", data.Data[0].ModelName)
	require.True(t, data.Data[0].ModelInfo.Capabilities.Chat)
	get := request("GET", "/v1/models/coding", "", openai.handleModelByID, true)
	require.Equal(t, 200, get.Code)
	get = request("GET", "/v1/models/coding@worker", "", openai.handleModelByID, true)
	require.Equal(t, 404, get.Code)
	show = request("POST", "/api/show", `{"model":"coding@worker"}`, ollama.handleOllamaShow, true)
	require.NotEqual(t, 200, show.Code)
	// Direct callers keep one bare identity when a physical model and group collide.
	tags = request("GET", "/api/tags", "", ollama.handleOllamaTags, false)
	require.NoError(t, json.Unmarshal(tags.Body.Bytes(), &list))
	require.Len(t, list.Models, 2)
}

func TestRouteDeleteRequiresTeamUnassignment(t *testing.T) {
	rig, _, s := routePolicyRig(t)
	store := modelgroup.NewGroupStore()
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))
	require.NoError(t, store.Set("coding", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "r", App: "ollama", Model: "local"}}}))
	ctrl := NewModelGroupsController(ModelGroupsDependencies{Groups: store, Access: s.access})
	r := gin.New()
	r.DELETE("/routes/:name", ctrl.DeleteModelGroup)
	remove := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("DELETE", "/routes/coding", nil))
		return w
	}
	require.Equal(t, 409, remove().Code)
	require.NotNil(t, store.Get("coding"))
	_, err := NewTeamsService(rig.teams, rig.keys, rig.access).UpdateTeam("team", &UpdateTeamRequest{RouteAccess: teams.RouteAccess{Mode: "direct"}}, "admin")
	require.NoError(t, err)
	require.Equal(t, 200, remove().Code)
	require.Nil(t, store.Get("coding"))
}
