package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
)

// TestSetAccessContext_StashesProxyLabels pins that every successful
// authentication propagates the alias/team labels into the
// proxy-metric context-stash. Regression-guards the
// zz.proxy.failed.requests.metric label-population contract: an auth
// path that omits any of these four c.Set calls would silently produce
// empty-label series for the affected dimension.
func TestSetAccessContext_StashesProxyLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	ac := &AccessContext{
		Key: &control.KeyPrincipal{
			ID:          "vk-alice-id",
			Name:        "Alice's production key",
			Fingerprint: "vkey...alic",
			Role:        "user",
			IsVirtual:   true,
		},
		Team: &control.TeamPrincipal{
			ID:         "team-engineering",
			Name:       "Engineering",
			MemberRole: teams.RoleOwner,
		},
	}
	setAccessContext(c, ac)

	cases := []struct {
		key, want string
	}{
		{proxy.CtxKeyAPIKeyAlias, "Alice's production key"},
		{proxy.CtxKeyHashedAPIKey, "vkey...alic"},
		{proxy.CtxKeyTeamID, "team-engineering"},
		{proxy.CtxKeyTeamAlias, "Engineering"},
	}
	for _, tc := range cases {
		v, ok := c.Get(tc.key)
		if !ok {
			t.Errorf("missing %s on gin context", tc.key)
			continue
		}
		s, _ := v.(string)
		if s != tc.want {
			t.Errorf("ctx[%s] = %q, want %q", tc.key, s, tc.want)
		}
	}
}

// TestSetAccessContext_NoTeam pins that a virtual key without a team
// affiliation populates the api-key labels but leaves the team labels
// unset — empty team_alias/team series are valid Prometheus labels and
// dashboards filtering on team="" handle the missing-team case.
func TestSetAccessContext_NoTeam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setAccessContext(c, &AccessContext{
		Key: &control.KeyPrincipal{
			ID:          "vk-bob-id",
			Name:        "Bob's key",
			Fingerprint: "vkey...bob1",
			Role:        "user",
			IsVirtual:   true,
		},
	})

	if _, ok := c.Get(proxy.CtxKeyAPIKeyAlias); !ok {
		t.Error("api_key_alias missing despite key set")
	}
	if _, ok := c.Get(proxy.CtxKeyTeamID); ok {
		t.Error("team_id should be absent when key has no team")
	}
	if _, ok := c.Get(proxy.CtxKeyTeamAlias); ok {
		t.Error("team_alias should be absent when key has no team")
	}
}

// TestSetAccessContext_NilSafety pins the early-return path: nil
// AccessContext, nil Key — the access_control.go anonymous branch can
// produce both — must not panic and must leave the proxy-stash keys
// unset.
func TestSetAccessContext_NilSafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setAccessContext(c, nil)              // should not panic
	setAccessContext(c, &AccessContext{}) // nil Key — should not panic

	if _, ok := c.Get(proxy.CtxKeyAPIKeyAlias); ok {
		t.Error("api_key_alias should be absent for nil-key context")
	}
}

// TestMirrorCallerIdentityToRecorder pins the gin -> recorder bridge:
// every recorder constructor (newChatRecorder / newCompletionRecorder /
// newEmbeddingsRecorder / newOllamaInferenceRecorder) calls this helper,
// and the values must round-trip onto the recorder's caller-identity
// fields so the inference-log bridge can emit the LiteLLM-vocabulary
// labels on zz.input.tokens.metric / zz.output.tokens.metric. A drift
// here would silently aggregate authenticated traffic into empty-label
// series — the kind of bug that only surfaces on dashboards.
func TestMirrorCallerIdentityToRecorder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	ac := &AccessContext{
		Key: &control.KeyPrincipal{
			ID:          "vk-alice-id",
			Name:        "Alice's production key",
			Fingerprint: "vkey...alic",
			Role:        "user",
			IsVirtual:   true,
		},
		Team: &control.TeamPrincipal{
			ID:         "team-engineering",
			Name:       "Engineering",
			MemberRole: teams.RoleOwner,
		},
	}
	setAccessContext(c, ac)

	rec := llm.NewInferenceRecorder(context.Background(), "llama3", "openai")
	mirrorCallerIdentityToRecorder(c, rec)

	// The recorder fields are unexported; observe them through the only
	// public surface that exposes them — InferenceLogData via
	// RecordCompletion + a stub hook. Any future field rename will surface
	// as a test failure here, not as a silent dashboard regression.
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(captureHook(func(d llm.InferenceLogData) { captured = d }))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	rec.RecordCompletion(1, 1)

	if captured.KeyAlias != "Alice's production key" {
		t.Errorf("KeyAlias = %q, want %q", captured.KeyAlias, "Alice's production key")
	}
	if captured.HashedKey != "vkey...alic" {
		t.Errorf("HashedKey = %q, want %q", captured.HashedKey, "vkey...alic")
	}
	if captured.TeamAlias != "Engineering" {
		t.Errorf("TeamAlias = %q, want %q", captured.TeamAlias, "Engineering")
	}
}

// TestMirrorCallerIdentityToRecorder_NilSafety pins that the helper
// no-ops on nil receivers — both legitimate during early-init paths
// and on test rigs that exercise recorders without a gin context.
func TestMirrorCallerIdentityToRecorder_NilSafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := llm.NewInferenceRecorder(context.Background(), "m", "p")
	mirrorCallerIdentityToRecorder(nil, rec) // nil ctx
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	mirrorCallerIdentityToRecorder(c, nil) // nil recorder
	mirrorCallerIdentityToRecorder(c, rec) // empty ctx — no panic, empty values
}

// captureHook is a tiny llm.InferenceLogHook that records the last
// InferenceLogData passed to it. Used to observe recorder fields that
// don't have public getters.
type captureHook func(llm.InferenceLogData)

func (h captureHook) OnInferenceComplete(d llm.InferenceLogData) { h(d) }
