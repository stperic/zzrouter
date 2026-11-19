package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
)

// newAuditTestServices wires both services to one store and returns it.
func newAuditTestServices(t *testing.T) (*KeysService, *TeamsService, access.Store) {
	t.Helper()
	store := newTestAccessStore(t)
	accessControl := newTestAccessControl(t, store)
	teamsSvc := NewTeamsService(store, accessControl)
	return NewKeysService(store, accessControl), teamsSvc, store
}

// auditHistory reads every event store recorded, oldest first, page by page.
func auditHistory(t *testing.T, store access.Store) []audit.Event {
	t.Helper()
	var events []audit.Event
	for cursor := ""; ; {
		page, next, err := store.AuditPage(t.Context(), cursor, 2)
		require.NoError(t, err)
		events = append(events, page...)
		if next == "" {
			slices.Reverse(events)
			return events
		}
		cursor = next
	}
}

// TestAudit_EndToEnd_ServiceMutationsEmitEvents pins that the three
// service-level mutations that drove the P2 #1 requirement — create
// key, delete key, create team — each record exactly one event in the
// store's history, in call order.
func TestAudit_EndToEnd_ServiceMutationsEmitEvents(t *testing.T) {
	keysSvc, teamsSvc, store := newAuditTestServices(t)

	// 1. Create a shared team.
	_, err := teamsSvc.CreateTeam(t.Context(), &CreateTeamRequest{
		ID:             "eng",
		Name:           "Engineering",
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}},
	}, "static:admin")
	require.NoError(t, err)

	// 2. Create a key into that team (not auto-personal, so no
	//    second team_created event fires).
	_, err = keysSvc.CreateKey(t.Context(), "alice", &CreateKeyRequest{
		Name:   "alice",
		TeamID: "eng",
	}, "static:admin")
	require.NoError(t, err)

	// 3. Delete the key. Note: cascade-delete of a personal team would
	//    fire an extra team.deleted event if this had been a personal
	//    team; here it's a shared-team member, so no cascade.
	require.NoError(t, keysSvc.DeleteKey(t.Context(), "alice", "static:admin"))

	events := auditHistory(t, store)
	actions := make([]string, 0, len(events))
	for _, ev := range events {
		actions = append(actions, string(ev.Action))
		require.Equal(t, "static:admin", ev.Actor)
		require.False(t, ev.Time.IsZero(), "every recorded event carries a timestamp")
	}
	require.Equal(t, []string{
		string(audit.ActionTeamCreated),
		string(audit.ActionKeyCreated),
		string(audit.ActionKeyDeleted),
	}, actions, "the history follows call order")

	// Subject checks pin the specific objects.
	require.Equal(t, "eng", events[0].Subject)
	require.Equal(t, map[string]any{"mode": "selected", "providers": []any{"ollama"}}, events[0].Metadata["provider_access"])
	require.Equal(t, "alice", events[1].Subject)
	require.Equal(t, "alice", events[2].Subject)
}

// TestAudit_PersonalTeamCascade pins that an auto-created personal team
// records team.created alongside key.created, and that deleting the owning
// key cascades into a team.deleted event. Without this coverage, a
// compliance reviewer would see key records without the corresponding
// team lifecycle.
func TestAudit_PersonalTeamCascade(t *testing.T) {
	keysSvc, _, store := newAuditTestServices(t)

	// Create without team_id → auto-creates personal team.
	_, err := keysSvc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "alice"}, "static:admin")
	require.NoError(t, err)
	// Delete the key → cascades to delete the personal team.
	require.NoError(t, keysSvc.DeleteKey(t.Context(), "alice", "static:admin"))

	events := auditHistory(t, store)
	actions := make([]string, 0, len(events))
	for _, ev := range events {
		actions = append(actions, string(ev.Action))
	}
	require.Equal(t, []string{
		string(audit.ActionTeamCreated), // personal team for alice
		string(audit.ActionKeyCreated),
		string(audit.ActionKeyDeleted),
		string(audit.ActionTeamDeleted), // cascade
	}, actions)
	// Cascade event should be marked as such in metadata.
	cascade := events[3]
	require.Equal(t, true, cascade.Metadata["cascade"], "cascade metadata flag must fire")
}

// TestAudit_UsageResetsAreRecorded pins the two events whose state lives in
// the spend ledger, not the store: they still reach the store's history.
func TestAudit_UsageResetsAreRecorded(t *testing.T) {
	keysSvc, teamsSvc, store := newAuditTestServices(t)
	_, err := teamsSvc.CreateTeam(t.Context(), &CreateTeamRequest{ID: "eng", Name: "Engineering"}, "static:admin")
	require.NoError(t, err)
	_, err = keysSvc.CreateKey(t.Context(), "alice", &CreateKeyRequest{Name: "alice", TeamID: "eng"}, "static:admin")
	require.NoError(t, err)

	require.NoError(t, keysSvc.ResetKeyUsage(t.Context(), "alice", "operator"))
	require.NoError(t, teamsSvc.ResetTeamUsage(t.Context(), "eng", ""))
	require.Error(t, keysSvc.ResetKeyUsage(t.Context(), "missing", "operator"))

	events := auditHistory(t, store)
	require.Len(t, events, 4, "a refused reset records nothing")
	require.Equal(t, audit.ActionKeyUsageReset, events[2].Action)
	require.Equal(t, "operator", events[2].Actor)
	require.Equal(t, "alice", events[2].Subject)
	require.Equal(t, audit.ActionTeamUsageReset, events[3].Action)
	require.Equal(t, audit.ActorStaticAdmin, events[3].Actor)
	require.Equal(t, "eng", events[3].Subject)
	require.Empty(t, events[3].RequestID)
}

// TestAccessAuditPagesThroughTheAPI reads people, key, team and usage-reset
// events through GET /access/audit, newest first across several pages, with
// their actors and request IDs, and refuses a cursor from another store.
func TestAccessAuditPagesThroughTheAPI(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeCoordinator)
	_, owner := createBrowserOwner(t, s, endpoint)
	for _, step := range []struct{ path, requestID string }{
		{"/zzrouter/v1/teams", "audit-team"},
		{"/zzrouter/v1/keys", "audit-key"},
		{"/zzrouter/v1/keys/alice/usage/reset", "audit-reset"},
	} {
		body := map[string]string{}
		switch step.path {
		case "/zzrouter/v1/teams":
			body = map[string]string{"id": "eng", "name": "Engineering"}
		case "/zzrouter/v1/keys":
			body = map[string]string{"id": "alice", "name": "Alice", "team_id": "eng"}
		}
		result := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: step.path, Body: body,
			Headers: map[string]string{"X-API-Key": TestAdminKey, "X-Request-ID": step.requestID}})
		require.Less(t, result.Code, 300, string(result.Body))
	}

	read := func(cursor string) (map[string]json.RawMessage, auditPage) {
		path := "/zzrouter/v1/access/audit?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		result := makeAuthRequest(t, s, http.MethodGet, path, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, result.Code, string(result.Body))
		var response struct {
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(result.Body, &response))
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(response.Data, &fields))
		var page auditPage
		require.NoError(t, json.Unmarshal(response.Data, &page))
		return fields, page
	}
	var events []audit.Event
	pages := 0
	for cursor := ""; ; pages++ {
		fields, page := read(cursor)
		require.NotContains(t, fields, "skipped", "a database skips no record")
		require.LessOrEqual(t, len(page.Events), 2)
		events = append(events, page.Events...)
		if page.Next == "" {
			require.NotContains(t, fields, "next", "the last page has no next cursor")
			break
		}
		cursor = page.Next
	}
	require.Greater(t, pages, 1, "the history spans several pages")
	actions := make([]audit.Action, len(events))
	for i, ev := range events {
		actions[i] = ev.Action
	}
	require.Equal(t, []audit.Action{audit.ActionKeyUsageReset, audit.ActionKeyCreated, audit.ActionTeamCreated, audit.ActionPersonCreated},
		actions[:4], "newest first")
	for _, ev := range events[:3] {
		require.Equal(t, audit.ActorStaticAdmin, ev.Actor)
		require.Empty(t, ev.RequestID, "key, team and usage events record no request ID, as before the store")
	}
	require.Equal(t, owner.ID, events[3].Actor, "people events name the person")
	require.NotEmpty(t, events[3].RequestID, "and the request")
	require.Equal(t, audit.ActionGrantIssued, events[len(events)-1].Action, "the setup grant came first")

	other, err := sqlstore.Open(t.Context(), t.TempDir(), sqlstore.Options{}) // a store of its own generation
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	for range 2 {
		other.Record(t.Context(), audit.Event{Action: audit.ActionKeyUsageReset, Actor: "other", Subject: "elsewhere"})
	}
	_, foreign, err := other.AuditPage(t.Context(), "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, foreign)
	stale := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/access/audit?cursor="+url.QueryEscape(foreign), TestAdminKey, nil)
	require.Equal(t, http.StatusConflict, stale.Code, "a cursor from another generation is refused")
}
