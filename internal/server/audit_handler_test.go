package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

const auditPath = "/zzrouter/v1/access/audit"

var auditEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// auditNode is a coordinator whose history holds exactly six events an hour
// apart. Callers are not parallel: it sets the process clock.
func auditNode(t *testing.T) *Server {
	t.Helper()
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	require.Empty(t, auditHistory(t, s.accessStore))
	for i := range 6 {
		restore := utils.SetClock(utils.FixedClock(auditEpoch.Add(time.Duration(i) * time.Hour)))
		require.NoError(t, s.accessStore.Record(t.Context(), audit.Event{Action: []audit.Action{audit.ActionKeyCreated, audit.ActionTeamUpdated}[i/3],
			Actor: []string{"alice", "bob"}[i%2], Subject: []string{"eng", "ops", "sales"}[i%3]}))
		restore()
	}
	return s
}

// readAudit pages through GET /access/audit under query, newest first.
func readAudit(t *testing.T, s *Server, query url.Values) []audit.Event {
	t.Helper()
	var events []audit.Event
	seen := map[string]bool{}
	for cursor := ""; ; {
		require.False(t, seen[cursor], "a cursor came back: paging would never end")
		seen[cursor] = true
		values := url.Values{"limit": {"2"}}
		for key, value := range query {
			values[key] = value
		}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?"+values.Encode(), TestAdminKey, nil)
		require.Equal(t, http.StatusOK, result.Code, string(result.Body))
		var response struct {
			Data auditPage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(result.Body, &response))
		events = append(events, response.Data.Events...)
		if response.Data.Next == "" {
			return events
		}
		cursor = response.Data.Next
	}
}

// auditLines names each event by actor, subject, action and hours after auditEpoch.
func auditLines(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = fmt.Sprint(ev.Actor, "/", ev.Subject, "/", ev.Action, "@", ev.Time.Sub(auditEpoch).Hours())
	}
	return out
}

// TestAccessAuditFiltersThroughTheAPI maps each parameter onto the store's
// filter across pages, and refuses a cursor under another filter with 409.
func TestAccessAuditFiltersThroughTheAPI(t *testing.T) {
	s := auditNode(t)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"actor=alice", []string{"alice/ops/team.updated@4", "alice/sales/key.created@2", "alice/eng/key.created@0"}},
		{"subject=ops", []string{"alice/ops/team.updated@4", "bob/ops/key.created@1"}},
		{"action=team.updated", []string{"bob/sales/team.updated@5", "alice/ops/team.updated@4", "bob/eng/team.updated@3"}},
		{"since=2026-11-01T04:00:00Z", []string{"bob/sales/team.updated@5", "alice/ops/team.updated@4"}},
		{"until=2026-11-01T01:00:00Z", []string{"alice/eng/key.created@0"}},
		{"since=2026-10-31T21:00:00-05:00&until=2026-11-01T04:00:00Z", []string{"bob/eng/team.updated@3", "alice/sales/key.created@2"}},
		{"since=2026-11-01T06:00:00%2B03:00&until=2026-11-01T04:00:00Z", []string{"bob/eng/team.updated@3"}},
		{"since=2026-11-01T00:00:00.000000001Z&until=2026-11-01T02:00:00.000000001Z", []string{"alice/sales/key.created@2", "bob/ops/key.created@1"}},
		{"actor=bob&subject=ops&action=key.created&since=2026-11-01T00:00:00Z&until=2026-11-01T05:00:00Z", []string{"bob/ops/key.created@1"}},
		{"actor=nobody", []string{}},
		{"since=2026-11-01T02:00:00Z&until=2026-11-01T02:00:00Z", []string{}},
		{"until=0001-01-01T00:00:00Z", []string{}},
	} {
		query, err := url.ParseQuery(tc.query)
		require.NoError(t, err)
		assert.Equal(t, tc.want, auditLines(readAudit(t, s, query)), tc.query)
	}

	issued := func(query string) string {
		result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=1&"+query, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, result.Code, string(result.Body))
		var response struct {
			Data auditPage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(result.Body, &response))
		require.NotEmpty(t, response.Data.Next, query)
		require.LessOrEqual(t, len(response.Data.Next), auditValueMax-64, "a filtered cursor fits the parameter bound with room for any position")
		return url.QueryEscape(response.Data.Next)
	}
	for _, tc := range []struct{ issuer, reader string }{
		{"actor=alice", "actor=bob"},
		{"actor=alice", "subject=eng"},
		{"actor=alice", ""},
		{"since=2026-11-01T00:00:00Z", "since=2026-11-01T01:00:00Z"},
		{"subject=sales", "subject=sales&action=team.updated"},
	} {
		cursor := issued(tc.issuer)
		stale := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=1&cursor="+cursor+"&"+tc.reader, TestAdminKey, nil)
		require.Equal(t, http.StatusConflict, stale.Code, "issued under %q, read under %q: %s", tc.issuer, tc.reader, stale.Body)
		assert.Equal(t, "conflict", problemCode(t, stale.Body))
		same := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=1&cursor="+cursor+"&"+tc.issuer, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, same.Code, "%s: %s", tc.issuer, same.Body)
	}
}

// TestAccessAuditRefusesBadParameters answers every refused parameter with a
// 400 Problem whose entry names the key and a closed code, before any read.
func TestAccessAuditRefusesBadParameters(t *testing.T) {
	s := auditNode(t)
	long := strings.Repeat("x", auditValueMax+1)
	actions := make([]string, 0, len(audit.Actions()))
	for _, action := range audit.Actions() {
		actions = append(actions, string(action))
	}
	for _, tc := range []struct {
		query string
		want  utils.ParamError
	}{
		{"bogus=1", utils.ParamError{Key: "bogus", Code: "unknown_field", Want: "one of: limit, cursor, actor, subject, action, since, until"}},
		{"Actor=alice", utils.ParamError{Key: "Actor", Code: "unknown_field", Want: "one of: limit, cursor, actor, subject, action, since, until"}},
		{"actor=alice&actor=bob", utils.ParamError{Key: "actor", Code: "invalid_value", Want: "one value"}},
		{"limit=2&limit=2", utils.ParamError{Key: "limit", Code: "invalid_value", Want: "one value"}},
		{"actor=", utils.ParamError{Key: "actor", Code: "invalid_value", Want: "a nonempty value"}},
		{"cursor=", utils.ParamError{Key: "cursor", Code: "invalid_value", Want: "a nonempty value"}},
		{"limit", utils.ParamError{Key: "limit", Code: "invalid_value", Want: "a nonempty value"}},
		{"subject=" + long, utils.ParamError{Key: "subject", Code: "out_of_range", Max: float64(auditValueMax)}},
		{"cursor=" + long, utils.ParamError{Key: "cursor", Code: "out_of_range", Max: float64(auditValueMax)}},
		{"action=key.create", utils.ParamError{Key: "action", Code: "invalid_value", Want: "one of: " + strings.Join(actions, ", ")}},
		{"since=yesterday", utils.ParamError{Key: "since", Code: "invalid_value", Want: "an RFC 3339 date-time"}},
		{"until=2026-11-01", utils.ParamError{Key: "until", Code: "invalid_value", Want: "an RFC 3339 date-time"}},
		{"since=2026-11-01T00:00:00+00:00", utils.ParamError{Key: "since", Code: "invalid_value", Want: "an RFC 3339 date-time",
			Hint: "a + in a query string decodes as a space: encode the offset's + as %2B"}},
		{"since=2026-11-02T00:00:00Z&until=2026-11-01T23:59:59Z", utils.ParamError{Key: "since", Code: "invalid_value", Want: "at or before until"}},
		{"limit=0", utils.ParamError{Key: "limit", Code: "out_of_range", Min: float64(1), Max: float64(100)}},
		{"limit=101", utils.ParamError{Key: "limit", Code: "out_of_range", Min: float64(1), Max: float64(100)}},
		{"limit=ten", utils.ParamError{Key: "limit", Code: "wrong_type", Want: "an integer"}},
		{"actor=%zz", utils.ParamError{Code: "invalid_value", Want: "a URL-encoded query"}},
	} {
		result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?"+tc.query, TestAdminKey, nil)
		require.Equal(t, http.StatusBadRequest, result.Code, "%s: %s", tc.query, result.Body)
		assert.Equal(t, "application/problem+json", result.Headers.Get("Content-Type"), tc.query)
		var problem utils.ProblemDetails
		require.NoError(t, json.Unmarshal(result.Body, &problem), tc.query)
		require.Len(t, problem.Errors, 1, "%s: %s", tc.query, result.Body)
		got := problem.Errors[0]
		assert.NotEmpty(t, got.Message, tc.query)
		got.Message = ""
		assert.Equal(t, tc.want, got, tc.query)
		assert.Equal(t, tc.want.Code, problem.Code, "%s: one refusal repeats its code", tc.query)
	}

	result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=0&bogus=1&since=soon", TestAdminKey, nil)
	require.Equal(t, http.StatusBadRequest, result.Code)
	var problem utils.ProblemDetails
	require.NoError(t, json.Unmarshal(result.Body, &problem))
	keys := make([]string, len(problem.Errors))
	for i, e := range problem.Errors {
		keys[i] = e.Key + ":" + e.Code
	}
	assert.Equal(t, []string{"bogus:unknown_field", "limit:out_of_range", "since:invalid_value"}, keys, "every refused key, in one answer")
	assert.Equal(t, "bad_request", problem.Code, "refusals that differ keep the status code")

	for _, query := range []string{"subject=" + long[1:], "limit=100"} {
		result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?"+query, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, result.Code, "%s: %s", query, result.Body)
	}
}

// TestAccessAuditUnfilteredResponseIsUnchanged pins the unfiltered answer
// byte for byte: the events newest first in commit order, as before filters
// existed, and a cursor naming the last one's time and seq. A cursor of the
// earlier seq-only form is refused, never misread.
func TestAccessAuditUnfilteredResponseIsUnchanged(t *testing.T) {
	s := auditNode(t)
	store, ok := s.accessStore.(*sqlstore.Store)
	require.True(t, ok)
	generation := store.Meta().Generation
	total := len(auditHistory(t, store))
	cursor := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"g":%q,"t":%d,"s":%d}`, generation, auditEpoch.Add(4*time.Hour).UnixMicro(), total-1))
	want := `{"success":true,"message":"Audit history retrieved successfully","data":{"events":[` +
		`{"time":"2026-11-01T05:00:00Z","action":"team.updated","actor":"bob","subject":"sales"},` +
		`{"time":"2026-11-01T04:00:00Z","action":"team.updated","actor":"alice","subject":"ops"}],"next":"` + cursor + `"}}`
	result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=2", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, result.Code)
	assert.Equal(t, want, string(result.Body))
	next := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=1&cursor="+cursor, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, next.Code)
	assert.Contains(t, string(next.Body), `"actor":"bob","subject":"eng"`)
	earlier := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"g":%q,"b":%d}`, generation, total-1))
	stale := makeAuthRequest(t, s, http.MethodGet, auditPath+"?limit=1&cursor="+earlier, TestAdminKey, nil)
	require.Equal(t, http.StatusConflict, stale.Code, "a cursor of the seq-only form: %s", stale.Body)
}

// TestAccessAuditGatingPrecedesValidation: filters change nothing about who
// may read the history or where; a refused caller learns nothing about its query.
func TestAccessAuditGatingPrecedesValidation(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeCoordinator)
	worker, _ := oauthTestNode(t, config.ClusterModeWorker)
	unfiltered := makeAuthRequest(t, worker, http.MethodGet, auditPath, TestAdminKey, nil).Code
	require.NotEqual(t, http.StatusOK, unfiltered)
	for _, query := range []string{"actor=alice", "bogus=1", "limit=0"} {
		for _, credential := range []struct {
			key  string
			code int
		}{{"", http.StatusUnauthorized}, {TestUserKey, http.StatusForbidden}} {
			result := makeAuthRequest(t, s, http.MethodGet, auditPath+"?"+query, credential.key, nil)
			assert.Equal(t, credential.code, result.Code, "%q with key %q: %s", query, credential.key, result.Body)
		}
		assert.Equal(t, unfiltered, makeAuthRequest(t, worker, http.MethodGet, auditPath+"?"+query, TestAdminKey, nil).Code, "worker: %s", query)
	}
}

// TestAccessAuditSpecMatchesHandler: the spec declares exactly the parameters
// the handler accepts, and the action enum is audit.Actions.
func TestAccessAuditSpecMatchesHandler(t *testing.T) {
	paths, ok := mapAt(loadSpecTree(t), "paths")
	require.True(t, ok)
	operation, ok := mapAt(paths[auditPath], "get")
	require.True(t, ok)
	var names, actions, enum []string
	for _, raw := range operation["parameters"].([]any) {
		parameter := raw.(map[string]any)
		names = append(names, parameter["name"].(string))
		if parameter["name"] == "action" {
			for _, value := range parameter["schema"].(map[string]any)["enum"].([]any) {
				enum = append(enum, value.(string))
			}
		}
	}
	for _, action := range audit.Actions() {
		actions = append(actions, string(action))
	}
	assert.Equal(t, auditParams, names)
	assert.ElementsMatch(t, actions, enum)
}
