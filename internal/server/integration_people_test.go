package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

const personPassword = "correct test horse battery staple"

func TestLocalRecoveryHostSupportsLoopbackBindNames(t *testing.T) {
	for input, expected := range map[string]string{"localhost": "127.0.0.1", "127.0.0.1": "127.0.0.1", "0.0.0.0": "127.0.0.1", "": "127.0.0.1", "::": "::1", "::1": "::1"} {
		host, err := localRecoveryHost(t.Context(), input)
		require.NoError(t, err)
		require.Equal(t, expected, host)
	}
}

func TestIntegrationPeopleSelfMutationKeepsAuthenticatedRevision(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	_, person := createBrowserOwner(t, s, endpoint)
	ref, err := s.access.core.PersonReference(person)
	require.NoError(t, err)
	_, err = s.accessStore.People().RevokeSessions(t.Context(), person.ID, person.Revision)
	require.NoError(t, err)
	// Represent a reset after middleware resolved the old session, but before
	// the handler constructs its password/revocation transaction.
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/zzrouter/v1/access/sessions/revoke", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), browserBindingKey{}, ref))
	require.Nil(t, s.currentPerson(c))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func anonymousPeopleBrowser(t *testing.T, endpoint *httptest.Server) *sessionBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := *endpoint.Client()
	client.Jar = jar
	b := &sessionBrowser{client: &client, endpoint: endpoint.URL}
	status, body := b.call(t, "GET", "/zzrouter/v1/session", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	b.csrf = peopleResponse[oauth.SessionInfo](t, body).CSRF
	return b
}

func peopleResponse[T any](t *testing.T, body []byte) T {
	t.Helper()
	var response struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	return response.Data
}

func peopleCall[T any](t *testing.T, b *sessionBrowser, method, path string, input any) T {
	t.Helper()
	status, body := b.call(t, method, "/zzrouter/v1"+path, input, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(body))
	return peopleResponse[T](t, body)
}

func createBrowserOwner(t *testing.T, s *Server, endpoint *httptest.Server) (*sessionBrowser, *people.Person) {
	t.Helper()
	// Installation authority needs no browser login from the scenario's shared peer.
	issued := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/access/console-grants", TestAdminKey, struct{}{})
	require.Equal(t, http.StatusCreated, issued.Code, string(issued.Body))
	grant := peopleResponse[people.IssuedGrant](t, issued.Body)
	require.Equal(t, people.PurposeSetup, grant.Purpose)
	owner := anonymousPeopleBrowser(t, endpoint)
	bound := peopleCall[people.IssuedGrant](t, owner, "POST", "/access/console-grants/exchange", map[string]string{"token": grant.Token})
	info := peopleCall[oauth.SessionInfo](t, owner, "POST", "/access/setup", map[string]string{"token": bound.Token, "username": "owner", "name": "Test Owner", "password": personPassword})
	require.True(t, info.Authenticated)
	owner.csrf = info.CSRF
	require.True(t, s.accessStore.Snapshot().Initialized())
	return owner, s.accessStore.Snapshot().Owner()
}

func TestIntegrationPeopleSecondSetupGrantReusesWorkspace(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	var teamIDs []string
	for range 2 {
		issued := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/access/console-grants", TestAdminKey, struct{}{})
		require.Equal(t, http.StatusCreated, issued.Code, string(issued.Body))
		grant := peopleResponse[people.IssuedGrant](t, issued.Body)
		require.Equal(t, people.PurposeSetup, grant.Purpose)
		teamIDs = append(teamIDs, grant.TeamID)
	}
	require.Equal(t, teamIDs[0], teamIDs[1], "a second setup grant targets the same workspace")
	workspaces := 0
	for _, team := range s.accessStore.Snapshot().Teams() {
		if team.Metadata["owner_workspace"] == "true" {
			workspaces++
		}
	}
	require.Equal(t, 1, workspaces)
}

// An invitation and the deletion of its team are serialized: either the team
// survives with the pending invitation, or the invitation is refused.
func TestIntegrationPeopleInvitationAndTeamDeletionSerialize(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	owner, _ := createBrowserOwner(t, s, endpoint)
	for i := range 8 {
		id := "invited-" + strconv.Itoa(i)
		_, err := s.services.Teams.CreateTeam(t.Context(), &CreateTeamRequest{ID: id, Name: id}, "admin")
		require.NoError(t, err)
		start := make(chan struct{})
		var invited, deleted int
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			invited, _ = owner.call(t, http.MethodPost, "/zzrouter/v1/access/invitations", map[string]string{"team_id": id, "role": "user"}, nil)
		})
		wg.Go(func() {
			<-start
			deleted, _ = owner.call(t, http.MethodDelete, "/zzrouter/v1/teams/"+id, nil, nil)
		})
		close(start)
		wg.Wait()
		snap := s.accessStore.Snapshot()
		if invited == http.StatusCreated {
			require.Equal(t, http.StatusConflict, deleted)
			require.NotNil(t, snap.Team(id))
			require.Equal(t, 1, snap.TeamImpact(id).Invitations)
		} else {
			require.Equal(t, http.StatusBadRequest, invited)
			require.Equal(t, http.StatusOK, deleted)
			require.Nil(t, snap.Team(id))
			require.Zero(t, snap.TeamImpact(id).Invitations, "no invitation outlives its team")
		}
	}
}

func TestIntegrationPeopleSetupInvitationsAndOwnerProtection(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	owner, person := createBrowserOwner(t, s, endpoint)
	identity := peopleCall[browserIdentity](t, owner, "GET", "/me", nil)
	require.Equal(t, "person", identity.Kind)
	require.True(t, identity.Owner)
	require.Equal(t, person.TeamID, identity.TeamID)
	status, _ := owner.call(t, "PATCH", "/zzrouter/v1/teams/"+person.TeamID, map[string]bool{"suspended": true}, nil)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = owner.call(t, "DELETE", "/zzrouter/v1/teams/"+person.TeamID, nil, nil)
	require.Equal(t, http.StatusConflict, status)
	status, _ = owner.call(t, "PATCH", "/zzrouter/v1/access/people/"+person.ID, map[string]string{"role": "user"}, nil)
	require.Equal(t, http.StatusForbidden, status)
	invite := peopleCall[people.IssuedGrant](t, owner, "POST", "/access/invitations", map[string]string{"team_id": person.TeamID, "role": "user"})
	member := anonymousPeopleBrowser(t, endpoint)
	bound := peopleCall[people.IssuedGrant](t, member, "POST", "/access/console-grants/exchange", map[string]string{"token": invite.Token})
	other := anonymousPeopleBrowser(t, endpoint)
	input := map[string]string{"token": bound.Token, "username": "teammate", "name": "Teammate", "password": personPassword}
	status, _ = other.call(t, "POST", "/zzrouter/v1/access/invitations/accept", input, nil)
	require.Equal(t, http.StatusBadRequest, status, "grant is bound to the redeeming browser")
	info := peopleCall[oauth.SessionInfo](t, member, "POST", "/access/invitations/accept", input)
	member.csrf = info.CSRF
	identity = peopleCall[browserIdentity](t, member, "GET", "/me", nil)
	require.Equal(t, RoleUser, identity.Role)
	require.Equal(t, person.TeamID, identity.TeamID)
	status, _ = member.call(t, "GET", "/zzrouter/v1/access/people", nil, nil)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = other.call(t, "POST", "/zzrouter/v1/access/console-grants/exchange", map[string]string{"token": invite.Token}, nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = owner.call(t, "PATCH", "/zzrouter/v1/access/onboarding", map[string]bool{"response_verified": true}, nil)
	require.Equal(t, http.StatusBadRequest, status, "browser cannot fabricate response evidence")
}

func TestIntegrationPeoplePasswordRevocationAndRecovery(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	owner, person := createBrowserOwner(t, s, endpoint)
	second := anonymousPeopleBrowser(t, endpoint)
	info := peopleCall[oauth.SessionInfo](t, second, "POST", "/session/password", map[string]string{"username": "owner", "password": personPassword})
	second.csrf = info.CSRF
	info = peopleCall[oauth.SessionInfo](t, owner, "POST", "/access/sessions/revoke", struct{}{})
	owner.csrf = info.CSRF
	status, _ := second.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	proof, err := localproof.Read(t.Context(), s.auditConfigDir)
	require.NoError(t, err)
	postProof := func(value, origin string) (int, []byte) {
		data, err := json.Marshal(map[string]string{"proof": value})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint.URL+"/zzrouter/v1/access/recovery/local", bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		response, err := endpoint.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, body
	}
	status, _ = postProof(proof.Proof, endpoint.URL)
	require.Equal(t, http.StatusForbidden, status, "a browser cannot invoke host recovery")
	status, _ = postProof("wrong-proof", "")
	require.Equal(t, http.StatusForbidden, status)
	status, body := postProof(proof.Proof, "")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), proof.Proof)
	grant := peopleResponse[people.IssuedGrant](t, body)
	recovery := anonymousPeopleBrowser(t, endpoint)
	bound := peopleCall[people.IssuedGrant](t, recovery, "POST", "/access/console-grants/exchange", map[string]string{"token": grant.Token})
	info = peopleCall[oauth.SessionInfo](t, recovery, "POST", "/access/recovery", map[string]string{"token": bound.Token, "password": "new correct test passphrase"})
	recovery.csrf = info.CSRF
	require.Equal(t, person.ID, s.accessStore.Snapshot().Owner().ID)
	require.Equal(t, person.TeamID, s.accessStore.Snapshot().Owner().TeamID)
	status, _ = owner.call(t, "GET", "/zzrouter/v1/me", nil, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = second.call(t, "POST", "/zzrouter/v1/session/password", map[string]string{"username": "owner", "password": personPassword}, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	peopleCall[browserIdentity](t, recovery, "GET", "/me", nil)
}

func TestIntegrationPeopleSetupRequiresInstallationAuthority(t *testing.T) {
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	visitor := anonymousPeopleBrowser(t, endpoint)
	status, _ := visitor.call(t, "POST", "/zzrouter/v1/access/setup", map[string]string{"username": "attacker", "name": "Visitor", "password": personPassword, "token": "made-up"}, nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.False(t, s.accessStore.Snapshot().Initialized())
	user := newSessionBrowser(t, endpoint, TestUserKey)
	grant := peopleCall[people.IssuedGrant](t, user, "POST", "/access/console-grants", struct{}{})
	require.Equal(t, people.PurposeSession, grant.Purpose)
	peopleCall[map[string]bool](t, visitor, "POST", "/access/console-grants/exchange", map[string]string{"token": grant.Token})
	state := peopleCall[oauth.SessionInfo](t, visitor, "GET", "/session", nil)
	visitor.csrf = state.CSRF
	identity := peopleCall[browserIdentity](t, visitor, "GET", "/me", nil)
	require.Equal(t, RoleUser, identity.Role)
	require.False(t, s.accessStore.Snapshot().Initialized())
}
