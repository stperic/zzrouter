package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/fallback"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mountPreviewRouter(t *testing.T, yamlBody string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(yamlBody)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))

	ctrl := NewModelGroupsController(store, nil, nil, nil, nil, nil, nil, nil, "/model-groups/")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
		c.Next()
	})
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)
	return r
}

const previewTwoRouteYAML = `
version: "1"
model_groups:
  fast-chat:
    strategy: priority
    replicas:
      - name: r1
        model: m
        provider: ollama
        priority: 1
      - name: r2
        model: m
        provider: vllm
        priority: 2
  slow-chat:
    strategy: priority
    replicas:
      - name: r3
        model: m
        provider: vllm
        priority: 1
`

func postPreview(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodePreview(t *testing.T, w *httptest.ResponseRecorder) previewResponse {
	t.Helper()
	var env struct {
		Data previewResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body=%s", w.Body.String())
	return env.Data
}

func TestPreview_SingleRoute_PicksByPriority(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	w := postPreview(t, r, "/zzrouter/v1/model-groups/fast-chat/preview", "{}")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	resp := decodePreview(t, w)
	require.Len(t, resp.Assignments, 1)
	a := resp.Assignments[0]
	assert.Equal(t, "fast-chat", a.Route)
	assert.Equal(t, "r1", a.Replica, "priority strategy must pick the lowest-Priority replica")
	assert.Equal(t, "priority", a.Strategy)
	assert.False(t, a.NoSurvivors)
}

func TestPreview_SingleRoute_MissingReturnsEmpty(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	w := postPreview(t, r, "/zzrouter/v1/model-groups/does-not-exist/preview", "{}")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	assert.Len(t, resp.Assignments, 0, "absent route → empty assignments")
}

func TestPreview_TopLevel_WalksAllAlphabetically(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	w := postPreview(t, r, "/zzrouter/v1/model-groups/preview", "")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	require.Len(t, resp.Assignments, 2)
	assert.Equal(t, "fast-chat", resp.Assignments[0].Route)
	assert.Equal(t, "slow-chat", resp.Assignments[1].Route)
	assert.False(t, resp.Capped)
}

func TestPreview_TopLevel_RespectsCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: \"1\"\nmodel_groups:\n")
	for i := 0; i < previewAssignmentsCap+5; i++ {
		fmt.Fprintf(&b, "  route-%02d:\n    strategy: priority\n    replicas:\n      - name: r1\n        model: m\n        provider: ollama\n        priority: 1\n", i)
	}
	r := mountPreviewRouter(t, b.String())

	w := postPreview(t, r, "/zzrouter/v1/model-groups/preview", "")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	assert.Len(t, resp.Assignments, previewAssignmentsCap)
	assert.True(t, resp.Capped, "exceeding cap must signal capped=true")
}

func TestPreview_ExcludeReplicasFiltersOut(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	body := `{"exclude_replicas":["r1"]}`
	w := postPreview(t, r, "/zzrouter/v1/model-groups/fast-chat/preview", body)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	require.Len(t, resp.Assignments, 1)
	a := resp.Assignments[0]
	assert.Equal(t, "r2", a.Replica, "r1 excluded → priority falls to r2")
	require.Len(t, a.Skipped, 1)
	assert.Equal(t, "r1", a.Skipped[0].Replica)
	assert.Equal(t, "excluded", a.Skipped[0].Reason)
}

func TestPreview_AllExcludedYieldsNoSurvivors(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	body := `{"exclude_replicas":["r1","r2"]}`
	w := postPreview(t, r, "/zzrouter/v1/model-groups/fast-chat/preview", body)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	require.Len(t, resp.Assignments, 1)
	a := resp.Assignments[0]
	assert.True(t, a.NoSurvivors)
	assert.Equal(t, "", a.Replica)
	assert.Len(t, a.Skipped, 2)
}

func TestPreview_NoPricingStoreLeavesSourceEmpty(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	body := `{"input_tokens":1000,"output_tokens":500}`
	w := postPreview(t, r, "/zzrouter/v1/model-groups/fast-chat/preview", body)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodePreview(t, w)
	require.Len(t, resp.Assignments, 1)
	a := resp.Assignments[0]
	assert.Equal(t, int64(0), a.ProjectedCostMicro)
	assert.Equal(t, "", a.ProjectedCostSource, "no pricing store → empty source")
	assert.Nil(t, resp.TotalProjectedCostMicro, "total omitted when any source is empty")
}

func TestStrategyName_NormalizesUnknown(t *testing.T) {
	// The store rejects unknown strategy values on Load, so this code
	// path is defensive. Kept as a unit assertion so a future relaxed
	// store validator doesn't accidentally leak a non-enum value onto
	// the preview wire shape.
	assert.Equal(t, "priority", strategyName(""))
	assert.Equal(t, "priority", strategyName(modelgroup.StrategyType("garbage")))
	assert.Equal(t, "least-load", strategyName(modelgroup.StrategyLeastLoad))
	assert.Equal(t, "fastest", strategyName(modelgroup.StrategyFastest))
}

func TestPickPreviewWinner_FastestPrefersLowerLatency(t *testing.T) {
	lat := &stubLatency{m: map[string]struct {
		dur     time.Duration
		samples int
	}{
		"r-fast": {50 * time.Millisecond, 3},
		"r-slow": {500 * time.Millisecond, 3},
	}}
	survivors := []fallback.Candidate{{Name: "r-slow"}, {Name: "r-fast"}}
	got := pickPreviewWinner(survivors, "fastest", lat, nil)
	assert.Equal(t, "r-fast", got.Name)
}

func TestPickPreviewWinner_FastestSendsUntriedToTheEnd(t *testing.T) {
	// An observed replica beats an untried one: strategy_fastest sorts
	// "Deployments with no data go to the end". The preview must agree,
	// or a dry-run recommends a replica live dispatch would not pick.
	lat := &stubLatency{m: map[string]struct {
		dur     time.Duration
		samples int
	}{
		"r-slow": {500 * time.Millisecond, 3},
		"r-new":  {0, 0},
	}}
	// Both orderings must reach the same winner — the incumbent must not
	// win merely by sitting first in the survivor list.
	for _, survivors := range [][]fallback.Candidate{
		{{Name: "r-slow"}, {Name: "r-new"}},
		{{Name: "r-new"}, {Name: "r-slow"}},
	} {
		got := pickPreviewWinner(survivors, "fastest", lat, nil)
		assert.Equal(t, "r-slow", got.Name)
	}
}

func TestPickPreviewWinner_FastestAllUntriedKeepsPriorityOrder(t *testing.T) {
	// With no data anywhere the live sort falls through to its priority
	// tiebreak; survivors arrive priority-sorted, so the first wins.
	lat := &stubLatency{m: map[string]struct {
		dur     time.Duration
		samples int
	}{}}
	survivors := []fallback.Candidate{{Name: "r-primary"}, {Name: "r-backup"}}
	got := pickPreviewWinner(survivors, "fastest", lat, nil)
	assert.Equal(t, "r-primary", got.Name)
}

func TestPickPreviewWinner_LeastLoadPrefersLowerInFlight(t *testing.T) {
	loads := stubLoad{"r-busy": 7, "r-idle": 1}
	survivors := []fallback.Candidate{{Name: "r-busy"}, {Name: "r-idle"}}
	got := pickPreviewWinner(survivors, "least-load", nil, loads)
	assert.Equal(t, "r-idle", got.Name)
}

type stubLatency struct {
	m map[string]struct {
		dur     time.Duration
		samples int
	}
}

func (s *stubLatency) Snapshot(name string) (time.Duration, int) {
	v, ok := s.m[name]
	if !ok {
		return 0, 0
	}
	return v.dur, v.samples
}

type stubLoad map[string]int64

func (s stubLoad) InFlight(name string) int64 { return s[name] }

func TestPreview_DeterministicAcrossCalls(t *testing.T) {
	r := mountPreviewRouter(t, previewTwoRouteYAML)

	body := `{"require_tags":[]}`
	w1 := postPreview(t, r, "/zzrouter/v1/model-groups/preview", body)
	w2 := postPreview(t, r, "/zzrouter/v1/model-groups/preview", body)
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "preview must be deterministic on identical state")
}
