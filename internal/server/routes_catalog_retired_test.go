package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/httperr"
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
}

// A flag is a claim. This drives every route the catalog flags and
// checks the server agrees, so "retired" cannot become a label on
// something that still answers.
func TestRouteCatalog_RetiredRoutesAnswerGone(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	driven, skipped := 0, 0
	for _, r := range s.engine.Routes() {
		if !isRetiredRoute(r.Handler) {
			continue
		}
		// The internal twins are unreachable to an HTTP caller by
		// design: internalRequestOnlyMiddleware 404s anything without
		// the in-process trust marker, so driving them here would test
		// that middleware, not the tombstone.
		if strings.HasPrefix(r.Path, adminInternalPrefix+"/") {
			skipped++
			continue
		}
		// The handler reads no parameters, so any placeholder that
		// occupies the segment reaches it.
		path := concreteRetiredPath(r.Path)
		resp := makeAuthRequest(t, s, r.Method, path, TestAdminKey, nil)
		require.Equal(t, http.StatusGone, resp.Code,
			"%s %s is flagged retired but answered %d: body=%s",
			r.Method, path, resp.Code, string(resp.Body))

		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &problem))
		assert.Equal(t, string(httperr.CodeRetired), problem.Code,
			"%s %s must carry the documented code, not a bare 410", r.Method, path)
		driven++
	}
	if driven == 0 {
		t.Fatal("drove no retired routes: isRetiredRoute stopped matching, or the " +
			"internal-prefix skip now swallows the public ones, and this guard " +
			"would pass vacuously")
	}
	t.Logf("drove %d retired routes to 410, skipped %d internal twins", driven, skipped)
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
