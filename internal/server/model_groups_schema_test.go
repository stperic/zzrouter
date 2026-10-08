package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

func TestRoutesSchema_AdvertisesAllClosedEnums(t *testing.T) {
	resp := routesSchema()
	requiredEnums := []string{
		"strategy", "cooldown_reason", "attempt_skip_reason",
		"attempt_outcome", "breaker_state", "health_state", "error_code",
		"capability", "steering_header", "steering_applied",
		"event_type",
	}
	for _, k := range requiredEnums {
		_, ok := resp.Enums[k]
		assert.True(t, ok, "enum %q must be advertised in schema", k)
	}
	// capability reserves its slot; populated when Phase 8 lands.
	assert.Empty(t, resp.Enums["capability"])
	// steering_header lists only the supported headers; the four deferred
	// ones (Require-Capabilities, Max-Cost-Micro, Prefer-Tags, Prefer-Model)
	// are NOT advertised — accepting them would lie to the agent.
	assert.ElementsMatch(t,
		[]string{"X-Route-Exclude-Replicas", "X-Route-Latency-Budget-Ms", "X-Route-Require-Tags"},
		resp.Enums["steering_header"],
	)
	assert.ElementsMatch(t,
		[]string{"exclude_replicas", "latency_budget_ms", "require_tags"},
		resp.Enums["steering_applied"],
	)
}

func TestRoutesSchema_AdvertisesSteeringErrorCodes(t *testing.T) {
	resp := routesSchema()
	codes := resp.Enums["error_code"]
	codeSet := make(map[string]bool)
	for _, c := range codes {
		codeSet[c] = true
	}
	assert.True(t, codeSet["invalid_steering_header"])
	assert.True(t, codeSet["unsupported_steering_header"])
}

func TestRoutesSchema_AdvertisesNewSkipReasons(t *testing.T) {
	resp := routesSchema()
	reasons := resp.Enums["attempt_skip_reason"]
	set := make(map[string]bool)
	for _, r := range reasons {
		set[r] = true
	}
	assert.True(t, set["excluded"], "attempt_skip_reason must include excluded (X-Route-Exclude-Replicas)")
	assert.True(t, set["latency_budget"], "attempt_skip_reason must include latency_budget (X-Route-Latency-Budget-Ms)")
}

func TestRoutesSchema_CooldownAndSkipReasonAreDistinct(t *testing.T) {
	// pkg/fallback exposes both cooldown reasons (what set the cooldown)
	// and skip reasons (why a candidate was filtered). The schema must
	// advertise them under separate keys — they're disjoint vocabularies.
	resp := routesSchema()
	cooldownSet := make(map[string]bool)
	for _, v := range resp.Enums["cooldown_reason"] {
		cooldownSet[v] = true
	}
	for _, v := range resp.Enums["attempt_skip_reason"] {
		assert.False(t, cooldownSet[v],
			"skip reason %q must not also appear under cooldown_reason", v)
	}
}

func TestRoutesSchema_ErrorCodeIncludesRouteCodes(t *testing.T) {
	resp := routesSchema()
	codes := resp.Enums["error_code"]
	want := []string{"route_unknown", "replica_unknown", "replica_last_in_group"}
	codeSet := make(map[string]bool)
	for _, c := range codes {
		codeSet[c] = true
	}
	for _, w := range want {
		assert.True(t, codeSet[w], "error_code enum must include %q (Phase 1 added it)", w)
	}
}

func TestSchemaEndpoint_StableETag(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	get := func(ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/schema", nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w1 := get("")
	require.Equal(t, http.StatusOK, w1.Code)
	etag := w1.Header().Get("ETag")
	require.NotEmpty(t, etag, "schema must carry an ETag")

	w2 := get(etag)
	assert.Equal(t, http.StatusNotModified, w2.Code,
		"second GET with If-None-Match must 304")
	assert.Equal(t, etag, w2.Header().Get("ETag"),
		"304 must echo the ETag so the client can keep caching")
}

func TestGetModelGroup_ETagStableAcrossPolls(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	get := func(ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat", nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w1 := get("")
	require.Equal(t, http.StatusOK, w1.Code)
	etag1 := w1.Header().Get("ETag")
	require.NotEmpty(t, etag1)

	// Second GET with no mutation in between must yield the same ETag —
	// the config projection excludes live derived fields, so cooldown
	// timing jitter doesn't perturb the hash.
	w2 := get("")
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, etag1, w2.Header().Get("ETag"))

	w3 := get(etag1)
	assert.Equal(t, http.StatusNotModified, w3.Code)
}

func TestGetModelGroup_ETagChangesAfterPatch(t *testing.T) {
	r := mountModelGroupsTestRouter(t, idemTestYAML)
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/fast-chat", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	etagBefore := get().Header().Get("ETag")
	require.NotEmpty(t, etagBefore)

	patchReq := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/fast-chat",
		strings.NewReader(`{"description":"changed"}`))
	patchReq.Header.Set("Content-Type", "application/merge-patch+json")
	wp := httptest.NewRecorder()
	r.ServeHTTP(wp, patchReq)
	require.Equal(t, http.StatusOK, wp.Code, "patch should succeed: %s", wp.Body.String())

	etagAfter := get().Header().Get("ETag")
	require.NotEmpty(t, etagAfter)
	assert.NotEqual(t, etagBefore, etagAfter,
		"PATCH that mutates description must invalidate the ETag")
}

func TestRoutesSchema_JSONIsDeterministic(t *testing.T) {
	// Marshaling the same schema twice must yield byte-identical bytes
	// so the ETag is stable. Catches map iteration order regressions.
	b1, err := json.Marshal(routesSchema())
	require.NoError(t, err)
	b2, err := json.Marshal(routesSchema())
	require.NoError(t, err)
	assert.Equal(t, string(b1), string(b2))
}

// The schema endpoint is how an agent learns which codes exist, so a
// gap here is worse than a missing test: a client builds a validator
// from this list and then treats a real code as unrecognised. The
// hand-maintained version had silently fallen two codes behind
// (unknown_field, route_not_claimed), which is exactly the failure this
// pins shut.
func TestErrorCodeEnum_AdvertisesEveryDeclaredCode(t *testing.T) {
	advertised := map[string]bool{}
	for _, c := range errorCodeEnum() {
		advertised[c] = true
	}
	for _, code := range httperr.AllParamErrorCodes() {
		if !advertised[string(code)] {
			t.Errorf("schema endpoint does not advertise %q; an agent building a local "+
				"validator from it would treat that code as unknown", code)
		}
	}
}

// The steering codes fire on the inference path rather than the mutator
// path, so they are appended by hand. Pinning them keeps that deliberate
// addition from being dropped during a refactor of the derived part.
func TestErrorCodeEnum_KeepsSteeringCodes(t *testing.T) {
	advertised := map[string]bool{}
	for _, c := range errorCodeEnum() {
		advertised[c] = true
	}
	for _, c := range []string{"invalid_steering_header", "unsupported_steering_header"} {
		if !advertised[c] {
			t.Errorf("schema endpoint dropped the steering code %q", c)
		}
	}
}

// Sorted output keeps the advertised list stable across restarts; a
// caller diffing the schema should see a change only when one happened.
func TestErrorCodeEnum_IsSorted(t *testing.T) {
	codes := errorCodeEnum()
	if !sort.StringsAreSorted(codes) {
		t.Errorf("errorCodeEnum must be sorted for a stable wire order, got %v", codes)
	}
}
