package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/config"
)

// A request malformed in itself answers 400 bad_request whatever the state of
// the records it names: missing, existing, or an ID already taken.
func TestMalformedAccessRequestsAnswer400WhateverTheRecords(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, seed := range []struct {
		path string
		body map[string]any
	}{
		{"/zzrouter/v1/teams", map[string]any{"id": "eng", "name": "Engineering"}},
		{"/zzrouter/v1/keys", map[string]any{"id": "k", "name": "K", "team_id": "eng"}},
		{"/zzrouter/v1/keys", map[string]any{"id": "solo", "name": "Solo"}},
	} {
		created := makeAuthRequest(t, s, http.MethodPost, seed.path, TestAdminKey, seed.body)
		require.Equal(t, http.StatusCreated, created.Code, string(created.Body))
	}
	with := func(base map[string]any, field string, value any) map[string]any {
		body := map[string]any{field: value}
		for k, v := range base {
			body[k] = v
		}
		return body
	}
	type request struct {
		method, path string
		body         map[string]any
	}
	var requests []request
	keyFields := map[string]any{"role": "root", "team_role": "boss", "reset_period": "hourly", "expires_at": "tomorrow"}
	for field, value := range keyFields {
		for _, target := range []map[string]any{
			{"id": "new", "name": "New", "team_id": "eng"},  // a free ID in an existing team
			{"id": "new", "name": "New", "team_id": "nope"}, // a missing team
			{"id": "k", "name": "K", "team_id": "eng"},      // an ID already taken
		} {
			requests = append(requests, request{http.MethodPost, "/zzrouter/v1/keys", with(target, field, value)})
		}
		if field == "team_role" {
			continue // not a PATCH field
		}
		for _, id := range []string{"k", "missing"} {
			requests = append(requests, request{http.MethodPatch, "/zzrouter/v1/keys/" + id, map[string]any{field: value}})
		}
	}
	for _, id := range []string{"k", "missing"} {
		requests = append(requests, request{http.MethodPatch, "/zzrouter/v1/keys/" + id, map[string]any{"role": ""}})
	}
	for _, id := range []string{"personal-new", "personal-solo"} {
		requests = append(requests, request{http.MethodPost, "/zzrouter/v1/teams", map[string]any{"id": id, "name": "Squatter"}})
	}
	teamFields := map[string]any{
		"route_access":    map[string]any{"mode": "selected"},
		"provider_access": map[string]any{"mode": "selected"},
		"reset_period":    "hourly",
	}
	for field, value := range teamFields {
		for _, id := range []string{"fresh", "eng"} {
			requests = append(requests, request{http.MethodPost, "/zzrouter/v1/teams", with(map[string]any{"id": id, "name": "Team"}, field, value)})
		}
		for _, id := range []string{"eng", "missing", "personal-solo"} {
			requests = append(requests, request{http.MethodPatch, "/zzrouter/v1/teams/" + id, map[string]any{field: value}})
		}
	}
	for _, r := range requests {
		name := fmt.Sprintf("%s %s %v", r.method, r.path, r.body)
		result := makeAuthRequest(t, s, r.method, r.path, TestAdminKey, r.body)
		require.Equal(t, http.StatusBadRequest, result.Code, "%s: %s", name, result.Body)
		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.Unmarshal(result.Body, &problem), name)
		require.Equal(t, "bad_request", problem.Code, name)
	}
}

// claimingStore commits an owner claim just before the setup grant's transaction.
type claimingStore struct {
	access.Store
	claim func()
}

func (c claimingStore) IssueSetup(ctx context.Context, issuer people.Issuer) (*people.IssuedGrant, error) {
	c.claim()
	return c.Store.IssueSetup(ctx, issuer)
}

// The console grant is decided inside its transaction: an owner claimed after
// the handler read the snapshot gets the installation a session code, not a 409.
func TestConsoleGrantGivesASessionCodeOnceSetupIsClaimed(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	store := s.accessStore
	ref, err := s.access.core.Reference(TestAdminKey)
	require.NoError(t, err)
	issuer, err := peopleIssuer(ref)
	require.NoError(t, err)
	earlier, err := store.IssueSetup(t.Context(), issuer)
	require.NoError(t, err)
	bound, err := store.People().Exchange(t.Context(), earlier.Token, "an earlier browser")
	require.NoError(t, err)
	s.accessStore = claimingStore{Store: store, claim: func() {
		_, err := store.People().ClaimOwner(context.Background(), bound.Token, "an earlier browser", people.CreatePerson{Username: "owner", Name: "Owner", Password: personPassword})
		require.NoError(t, err)
	}}
	issued := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/access/console-grants", TestAdminKey, struct{}{})
	require.Equal(t, http.StatusCreated, issued.Code, string(issued.Body))
	require.Equal(t, people.PurposeSession, peopleResponse[people.IssuedGrant](t, issued.Body).Purpose)
	require.True(t, store.Snapshot().Initialized())
}
