package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The route catalog is the enumeration an agent plans against, and it
// lists every mounted route including the parameter mutators that only
// answer 410 Gone. Unflagged, a gravestone reads exactly like a live
// route: the agent spends a request to find out, and if it retries on
// 4xx it spends several.
//
// Both directions are checked against the engine, so neither a new
// tombstone nor a live route wrongly flagged can slip through, and the
// wire key is read literally rather than through RouteSpec — a struct
// tag typo agrees with itself on both sides of a round-trip.

func TestRouteCatalog_FlagsExactlyTheRetiredRoutes(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	want := map[string]bool{}
	for _, r := range s.engine.Routes() {
		if isRetiredRoute(r.Handler) {
			want[r.Method+" "+r.Path] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("engine mounts no retired routes: isRetiredRoute stopped matching, " +
			"and this guard would pass vacuously")
	}
	t.Logf("engine mounts %d retired routes", len(want))

	resp := makeAuthRequest(t, s, http.MethodGet, apipath.ServerRoutes, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", string(resp.Body))

	var cat struct {
		Routes []map[string]any `json:"routes"`
		Count  int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &cat))
	require.Equal(t, cat.Count, len(cat.Routes), "count header must match the body")

	got := map[string]bool{}
	for _, r := range cat.Routes {
		if flag, ok := r["retired"].(bool); ok && flag {
			got[r["method"].(string)+" "+r["path"].(string)] = true
		}
	}
	assert.Equal(t, want, got,
		"the catalog's retired set must equal the engine's: a missing flag advertises "+
			"a dead end as callable, an extra one hides a live route")
	assert.True(t, got["POST /zzrouter/v1/keys/reload"], "key reload must be flagged retired")
}

// A flag is a claim. This drives every route the catalog flags and
// checks the server agrees, so "retired" cannot become a label on
// something that still answers.
func TestRouteCatalog_RetiredRoutesAnswerGone(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	public, internal := 0, 0
	for _, r := range s.engine.Routes() {
		if !isRetiredRoute(r.Handler) {
			continue
		}
		// The handler reads no parameters, so any placeholder that
		// occupies the segment reaches it.
		path := concreteRetiredPath(r.Path)
		t.Run(r.Method+" "+path, func(t *testing.T) {
			req := httptest.NewRequest(r.Method, path, nil)
			req.Header.Set("X-API-Key", TestAdminKey)
			req.Header.Set("X-Request-ID", "retired-route-request")
			if strings.HasPrefix(r.Path, adminInternalPrefix+"/") {
				// Exercise internal twins through their real dispatch trust marker.
				req = req.WithContext(context.WithValue(req.Context(), internalRequestKey, true))
				internal++
			} else {
				public++
			}
			resp := httptest.NewRecorder()
			s.engine.ServeHTTP(resp, req)
			require.Equal(t, http.StatusGone, resp.Code, "body=%s", resp.Body.String())
			assert.Equal(t, "application/problem+json", resp.Header().Get("Content-Type"))

			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
			assert.Equal(t, string(httperr.CodeRetired), problem.Code)
			assert.Equal(t, http.StatusGone, problem.Status)
			assert.Equal(t, "Gone", problem.Title)
			assert.Equal(t, "https://api.zzrouter.com/problems/gone", problem.Type)
			assert.Equal(t, path, problem.Instance)
			assert.Equal(t, "retired-route-request", problem.RequestID)
			wantDetail := "this route is retired; PATCH /zzrouter/v1/providers/{name}/parameters " +
				"with Content-Type: application/merge-patch+json is the single mutator"
			if path == "/zzrouter/v1/keys/reload" {
				wantDetail = "this route is retired; app keys are managed through the keys API: " +
					"POST /zzrouter/v1/keys, PATCH /zzrouter/v1/keys/{id}"
			}
			assert.Equal(t, wantDetail, problem.Detail)
		})
	}
	require.Equal(t, 10, public, "all nine parameter routes and key reload must be retired")
	require.Equal(t, 9, internal, "all nine internal parameter twins must be retired")
}

func TestDeletedStateRoutesAnswerNotFound(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, path := range []string{"/zzrouter/v1/state/snapshot", "/zzrouter/v1/state/restore"} {
		t.Run(path, func(t *testing.T) {
			resp := makeAuthRequest(t, s, http.MethodPost, path, TestAdminKey, nil)
			require.Equal(t, http.StatusNotFound, resp.Code, "body=%s", resp.Body)
			assert.Equal(t, "application/problem+json", resp.Headers.Get("Content-Type"))
			var problem utils.ProblemDetails
			require.NoError(t, json.Unmarshal(resp.Body, &problem))
			assert.Equal(t, http.StatusNotFound, problem.Status)
			assert.Equal(t, "not_found", problem.Code)
		})
	}
}

// concreteRetiredPath fills gin's parameter segments with a placeholder.
func concreteRetiredPath(tmpl string) string {
	segs := strings.Split(tmpl, "/")
	for i, seg := range segs {
		if len(seg) > 1 && (seg[0] == ':' || seg[0] == '*') {
			segs[i] = "placeholder"
		}
	}
	return strings.Join(segs, "/")
}
