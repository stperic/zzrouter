package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func providerPolicyRig(t *testing.T) (*accessRig, string, *Server) {
	t.Helper()
	rig := newAccessRig(t)
	rig.seedTeam(t, "team", &teams.Team{Name: "Team", ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"vllm"}}})
	raw := rig.seedKey(t, "key", "team", "member", &keys.VirtualKey{Name: "Client", Role: "user"})
	return rig, raw, &Server{access: rig.access}
}

func TestProviderAccessFiltersTargetsAndPolicyChanges(t *testing.T) {
	rig, raw, s := providerPolicyRig(t)
	c, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	candidates := []fallback.Candidate{{App: "groq", Model: "paid", Node: "coord"}, {App: "vllm", Model: "local", Node: "worker"}, {App: "ollama", Model: "other", Node: "worker"}}
	target := &resolver.Resolved{Candidates: candidates}
	require.True(t, s.admitAccessTargets(c, target))
	require.Len(t, target.Candidates, 1)
	require.Equal(t, "vllm", target.Provider)
	require.Equal(t, "local", target.ModelName)
	require.Equal(t, "groq", candidates[0].App, "shared candidates stay unchanged")
	for _, provider := range []string{"groq", ""} {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		require.False(t, s.admitAccessTargets(c, &resolver.Resolved{Provider: provider}))
		require.Equal(t, 403, w.Code)
	}
	svc := NewTeamsService(rig.teams, rig.keys, rig.access)
	renamed := "Renamed"
	_, err := svc.UpdateTeam("team", &UpdateTeamRequest{Name: &renamed}, "admin")
	require.NoError(t, err)
	require.Equal(t, "selected", rig.teams.Get("team").ProviderAccess.Mode)
	_, err = svc.UpdateTeam("team", &UpdateTeamRequest{ProviderAccess: teams.ProviderAccess{Mode: "none"}}, "admin")
	require.NoError(t, err)
	c, w := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.False(t, s.admitAccessTargets(c, &resolver.Resolved{Provider: "vllm"}))
	require.Equal(t, 403, w.Code, "cached authentication must observe updated team policy")
	require.True(t, rig.teams.HasGatedTeam())
}

func TestProviderAccessOpaqueAndOllamaManagement(t *testing.T) {
	rig, raw, s := providerPolicyRig(t)
	for _, provider := range []string{"", "ollama", "groq"} {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		require.False(t, s.admitOpaqueForward(c, "fixture", provider, false))
		require.Equal(t, 403, w.Code)
	}
	// No collaborators are installed: reaching a resolver, proxy or daemon would panic.
	// This also covers m@worker resolving to an allowed vLLM but operating Ollama.
	service := &OllamaServiceImpl{}
	for _, endpoint := range []string{"/api/copy", "/api/push", "/api/create"} {
		c, _ := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		c.Request.Body = http.NoBody
		err := service.ManagementCommand(c, endpoint, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "provider policy")
	}
	c, _ := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	require.Error(t, service.Deploy(c, "m@worker", false, nil))
	require.Error(t, service.CreateBlob(c, "sha256:fixture"))
	for _, handler := range []gin.HandlerFunc{(&OllamaHandlers{}).handleOllamaVersion, (&OllamaHandlers{}).handleOllamaMe, (&OllamaHandlers{}).handleOllamaBlobsHead} {
		c, w := rig.makeRequest(raw)
		require.True(t, rig.access.Authenticate(c, RoleUser))
		handler(c)
		require.Equal(t, 403, w.Code)
	}
}

func TestProviderAccessCatalogAndShowRoundTrip(t *testing.T) {
	rig, raw, s := providerPolicyRig(t)
	models := []*cache.CachedModel{{Name: "m", Model: "physical", Provider: "llamacpp", Node: "worker"}, {Name: "m", Model: "physical", Provider: "vllm", Node: "worker"}}
	cacheView := &mockModelCache{models: models}
	router := &recordingRouter{mockRouter: mockRouter{response: &routing.Response{StatusCode: 200, Body: []byte(`{"template":"fixture"}`)}}}
	service := NewModelService(cacheView, nil, nil, nil, router, nil)
	h := &OpenAIModelHandlers{appsConfig: noAppsConfig, localNode: func() string { return "test" }, modelService: service, access: s.catalogAllowed, adapter: NewOpenAIAdapter(func() *config.AppsConfig { return nil })}
	c, w := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	h.handleModels(c)
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "llamacpp")
	require.Contains(t, w.Body.String(), "vllm")
	_, err := service.ShowModel(t.Context(), &ShowModelRequest{ModelName: "m", Node: "worker", Admit: func(model, provider string) bool { return s.catalogAllowed(c, model, provider) }})
	require.NoError(t, err)
	require.Len(t, router.routed, 1)
	require.Contains(t, router.routed[0].Path, "provider=vllm")
	_, err = service.DeleteModels(t.Context(), &DeleteModelsRequest{Model: "m", Provider: "llamacpp", Admit: func(model, provider string) bool { return s.catalogAllowed(c, model, provider) }})
	require.Error(t, err)
	require.Len(t, router.routed, 1, "denied deletion never reaches a worker")
	require.Equal(t, "llamacpp", models[0].Provider)
}

func TestProviderAccessCatalogSourceAlias(t *testing.T) {
	rig, raw, s := providerPolicyRig(t)
	team := *rig.teams.Get("team")
	team.AllowedModels = []string{"org/model"}
	require.NoError(t, rig.teams.Update("team", &team))
	service := NewModelService(&mockModelCache{models: []*cache.CachedModel{{Name: "file-stem", SourceID: "org/model", Provider: "vllm", Node: "worker"}}}, nil, nil, nil, nil, nil)
	h := &OpenAIModelHandlers{appsConfig: noAppsConfig, localNode: func() string { return "test" }, modelService: service, access: s.catalogAllowed, adapter: NewOpenAIAdapter(func() *config.AppsConfig { return nil })}
	c, w := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	h.handleModels(c)
	var response struct {
		Data []OpenAIModelObject `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Data, 1)
	require.Equal(t, "org/model@worker", response.Data[0].ID)
	c, w = rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	c.Params = gin.Params{{Key: "model", Value: "/org/model@worker"}}
	h.handleModelByID(c)
	require.Equal(t, 200, w.Code)
}

func TestProviderAccessPsFiltersAndStripsMetadata(t *testing.T) {
	rig, raw, s := providerPolicyRig(t)
	h := &OllamaHandlers{access: s.catalogAllowed, handleRoute: func(c *gin.Context, cfg RoutingConfig) {
		body, err := cfg.Transform([]byte(`{"models":[{"name":"m@a","model":"m","provider":"ollama"},{"name":"m@b","model":"m","provider":"vllm"},{"name":"unknown@c","model":"m"}]}`))
		require.NoError(t, err)
		c.Data(200, "application/json", body)
	}}
	c, w := rig.makeRequest(raw)
	require.True(t, rig.access.Authenticate(c, RoleUser))
	h.handleOllamaPs(c)
	require.NotContains(t, w.Body.String(), "provider")
	require.NotContains(t, w.Body.String(), "m@a")
	require.NotContains(t, w.Body.String(), "unknown@c")
	require.Contains(t, w.Body.String(), "m@b")
}

func TestProviderAccessNativeWireUsesUpdatedPolicy(t *testing.T) {
	backend, recorded, mu := newRecordingBackend(t)
	s := createNativeWireTestNode(t, map[string]string{"vendor": backend.URL})
	response := makeAuthRequest(t, s, "POST", "/zzrouter/v1/teams", TestAdminKey, map[string]any{"id": "restricted", "name": "Restricted", "provider_access": map[string]any{"mode": "none"}})
	require.Equal(t, 201, response.Code, string(response.Body))
	raw := loadTestKey(t, s, "member", keys.VirtualKey{Name: "Member", TeamID: "restricted"})
	invoke := func() *TestResponse {
		return makeAuthRequest(t, s, "POST", "/vendor/test", raw, map[string]any{"model": "fixture"})
	}
	require.Equal(t, 403, invoke().Code)
	response = makeAuthRequest(t, s, "PATCH", "/zzrouter/v1/teams/restricted", TestAdminKey, map[string]any{"provider_access": map[string]any{"mode": "selected", "providers": []string{"vendor-backend"}}})
	require.Equal(t, 200, response.Code, string(response.Body))
	require.Equal(t, 200, invoke().Code)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, *recorded, 1)
	// Invalid explicit null must never widen a policy.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/teams/restricted", strings.NewReader(`{"provider_access":null}`))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Content-Type", "application/json")
	s.engine.ServeHTTP(w, req)
	require.Equal(t, 400, w.Code)
}
