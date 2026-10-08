package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Reserved names must be refused at create time, and only there.
//
// gin resolves a literal segment ahead of a :param sibling, so a
// resource named "schema" is created through the :param tree but then
// unreachable on the verbs where the literal exists. These tests drive
// each create surface with every name its collection reserves, plus a
// control proving a non-reserved name still goes through: a check that
// rejects everything would pass the reserved cases just as well.

const reservedNamesBaseYAML = `
version: "1"
model_groups:
  preview:
    strategy: priority
    replicas:
      - name: r1
        model: m
        provider: ollama
        priority: 1
`

func TestModelGroupPUT_RejectsReservedNamesOnCreate(t *testing.T) {
	body := `{"strategy":"priority","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`

	for name := range reservedModelGroupNames {
		if name == "preview" {
			continue // exercised by the grandfathering test below
		}
		t.Run(name, func(t *testing.T) {
			r := mountModelGroupsTestRouter(t, reservedNamesBaseYAML)
			req := httptest.NewRequest(http.MethodPut, "/zzrouter/v1/model-groups/"+name, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
			var errBody utils.ProblemDetails
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errBody))
			require.Len(t, errBody.Errors, 1)
			assert.Equal(t, string(httperr.CodeReservedName), errBody.Errors[0].Code)
			assert.Equal(t, string(httperr.CodeReservedName), errBody.Code,
				"the top-level code must repeat a single per-key failure")
			assert.Equal(t, name, errBody.Errors[0].Got)

			// The rejection must have created nothing: a group listed
			// here would be live for inference but unmanageable.
			list := httptest.NewRecorder()
			r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/zzrouter/v1/model-groups", nil))
			assert.NotContains(t, list.Body.String(), `"`+name+`"`)
		})
	}
}

// A group that already exists under a reserved name (predating the
// check, or an auto-route claimed under an empty route_prefix) must
// stay updatable: only creation is refused.
func TestModelGroupPUT_GrandfathersExistingReservedName(t *testing.T) {
	r := mountModelGroupsTestRouter(t, reservedNamesBaseYAML)
	body := `{"strategy":"priority","description":"updated","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	req := httptest.NewRequest(http.MethodPut, "/zzrouter/v1/model-groups/preview", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "an existing group under a reserved name lost updates: body=%s", w.Body.String())
}

func TestModelGroupPUT_AcceptsNonReservedName(t *testing.T) {
	r := mountModelGroupsTestRouter(t, reservedNamesBaseYAML)
	body := `{"strategy":"priority","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	req := httptest.NewRequest(http.MethodPut, "/zzrouter/v1/model-groups/fine-name", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

func TestTeamCreate_RejectsReservedIDs(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	for id := range reservedTeamIDs {
		resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
			map[string]any{"id": id, "name": "shadowed"})
		require.Equal(t, http.StatusBadRequest, resp.Code, "id=%s body=%s", id, resp.Body)
		assert.Contains(t, string(resp.Body), "is reserved")
	}

	resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/teams", TestAdminKey,
		map[string]any{"id": "research", "name": "control"})
	require.Equal(t, http.StatusCreated, resp.Code, "control create failed: body=%s", resp.Body)
}

func TestKeyCreate_RejectsReservedIDs(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	for id := range reservedKeyIDs {
		resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey,
			map[string]any{"id": id, "name": "shadowed"})
		require.Equal(t, http.StatusBadRequest, resp.Code, "id=%s body=%s", id, resp.Body)
		assert.Contains(t, string(resp.Body), "is reserved")
	}

	resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/keys", TestAdminKey,
		map[string]any{"id": "agent-7", "name": "control"})
	require.Equal(t, http.StatusCreated, resp.Code, "control create failed: body=%s", resp.Body)
}

func TestProviderInstanceCreate_RejectsReservedNames(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})

	for name := range reservedProviderNames {
		resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/providers/instances", TestAdminKey,
			map[string]any{"type": "ollama-connect", "name": name, "endpoint": "http://nas.lan:11434"})
		require.Equal(t, http.StatusBadRequest, resp.Code, "name=%s body=%s", name, resp.Body)
		assert.Contains(t, string(resp.Body), "is reserved")
	}

	resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/providers/instances", TestAdminKey,
		map[string]any{"type": "ollama-connect", "name": "ollama-nas", "endpoint": "http://nas.lan:11434"})
	require.Equal(t, http.StatusCreated, resp.Code, "control create failed: body=%s", resp.Body)
}

// The charset regex still fires ahead of the reserved check; the
// extraction into validateProviderInstanceName must not have lost it.
func TestProviderInstanceName_CharsetStillEnforced(t *testing.T) {
	msg := validateProviderInstanceName("Bad_Name")
	require.NotEmpty(t, msg)
	assert.True(t, strings.Contains(msg, "must match"), "msg=%s", msg)
}
