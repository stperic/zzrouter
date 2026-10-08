package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const autoManagedTestYAML = `
version: "1"
model_groups:
  route-llama3:
    description: "Route for llama3 across 2 replicas"
    auto_managed: true
    strategy: least-load
    replicas:
      - name: ollama-node-a
        model: llama3
        provider: ollama
        node: node-a
      - name: vllm-node-b
        model: llama3
        provider: vllm
        node: node-b
`

// TestAutoManagedGroup_MutationClaims pins the Phase 6 invariant: every
// user-facing mutator on an auto-managed group succeeds and stamps
// AutoOwner with the caller principal. The 409 + route_auto_managed
// refusal of Phase 5 is gone — implicit claim replaces it.
func TestAutoManagedGroup_MutationClaims(t *testing.T) {
	cases := []struct {
		name, method, path, body, contentType string
	}{
		{
			name:   "PUT group",
			method: "PUT", path: "/zzrouter/v1/model-groups/route-llama3",
			body:        `{"strategy":"priority","replicas":[{"name":"r","model":"m","provider":"ollama"}]}`,
			contentType: "application/json",
		},
		{
			name:   "PATCH group",
			method: "PATCH", path: "/zzrouter/v1/model-groups/route-llama3",
			body:        `{"description":"claimed"}`,
			contentType: "application/merge-patch+json",
		},
		{
			name:   "PUT replica",
			method: "PUT", path: "/zzrouter/v1/model-groups/route-llama3/replicas/ollama-node-a",
			body:        `{"model":"llama3","provider":"ollama"}`,
			contentType: "application/json",
		},
		{
			name:   "PATCH replica",
			method: "PATCH", path: "/zzrouter/v1/model-groups/route-llama3/replicas/ollama-node-a",
			body:        `{"priority":42}`,
			contentType: "application/merge-patch+json",
		},
		{
			name:   "DELETE replica",
			method: "DELETE", path: "/zzrouter/v1/model-groups/route-llama3/replicas/ollama-node-a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mountModelGroupsTestRouter(t, autoManagedTestYAML)
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

			// Probe state via GET.
			getReq := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/route-llama3", nil)
			getW := httptest.NewRecorder()
			r.ServeHTTP(getW, getReq)
			require.Equal(t, http.StatusOK, getW.Code)
			var got struct {
				Data struct {
					AutoManaged bool   `json:"auto_managed"`
					AutoOwner   string `json:"auto_owner"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &got))
			assert.False(t, got.Data.AutoManaged, "mutation must flip AutoManaged=false")
			assert.Equal(t, "alice", got.Data.AutoOwner, "mutation must stamp caller as AutoOwner")
		})
	}
}

// TestReleaseModelGroupOwner_ReversesClaim pins the DELETE /:name/owner
// contract: after a user claims via mutation, the release verb is the
// single path back to autoroute generation. Sets AutoManaged=true and
// clears AutoOwner. Idempotent on already-auto-managed groups.
func TestReleaseModelGroupOwner_ReversesClaim(t *testing.T) {
	r := mountModelGroupsTestRouter(t, autoManagedTestYAML)

	// Step 1: claim via PATCH.
	claimReq := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/route-llama3",
		bytes.NewBufferString(`{"description":"claimed"}`))
	claimReq.Header.Set("Content-Type", "application/merge-patch+json")
	claimW := httptest.NewRecorder()
	r.ServeHTTP(claimW, claimReq)
	require.Equal(t, http.StatusOK, claimW.Code)

	// Step 2: release.
	relReq := httptest.NewRequest("DELETE", "/zzrouter/v1/model-groups/route-llama3/owner", nil)
	relW := httptest.NewRecorder()
	r.ServeHTTP(relW, relReq)
	require.Equal(t, http.StatusOK, relW.Code, "body=%s", relW.Body.String())

	// Step 3: confirm state via GET.
	getReq := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/route-llama3", nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)
	var got struct {
		Data struct {
			AutoManaged bool   `json:"auto_managed"`
			AutoOwner   string `json:"auto_owner"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &got))
	assert.True(t, got.Data.AutoManaged, "release flips AutoManaged=true")
	assert.Equal(t, "", got.Data.AutoOwner, "release clears AutoOwner")
}

// TestAutoManagedGroup_ConcurrentPATCHesSerialize pins I1 from Phase 6
// cold review: ApplyPatch is RMW-under-lock so two parallel PATCHes
// against an auto-managed group serialize, the first wins the AutoOwner
// slot, and the second observes a normal (already-claimed) group.
func TestAutoManagedGroup_ConcurrentPATCHesSerialize(t *testing.T) {
	r := mountModelGroupsTestRouter(t, autoManagedTestYAML)

	var wg sync.WaitGroup
	const N = 8
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/route-llama3",
				bytes.NewBufferString(`{"description":"concurrent"}`))
			req.Header.Set("Content-Type", "application/merge-patch+json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
		}()
	}
	close(start)
	wg.Wait()

	getReq := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/route-llama3", nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)
	var got struct {
		Data struct {
			AutoManaged bool   `json:"auto_managed"`
			AutoOwner   string `json:"auto_owner"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &got))
	assert.False(t, got.Data.AutoManaged)
	// The harness sets principal to "alice"; all N PATCHes claim with
	// the same principal so AutoOwner is stable. The race-detector run
	// proves there's no torn write on the field.
	assert.Equal(t, "alice", got.Data.AutoOwner)
}

// userAuthoredTestYAML is a hand-rolled route: no auto_managed flag and
// no owner, the state DELETE /:name/owner must refuse to touch.
const userAuthoredTestYAML = `
version: "1"
model_groups:
  prod-chat:
    description: "hand-authored production route"
    strategy: priority
    replicas:
      - name: ollama-node-a
        model: llama3
        provider: ollama
        node: node-a
`

// TestReleaseModelGroupOwner_RefusesUnclaimedGroup pins that releasing a
// group nobody claimed is a 409, not a silent flip.
//
// Flipping a hand-authored group to AutoManaged parked it in a state with
// no way out: SyncAllFromCache would reap it as "model gone" and persist
// the deletion, while DeleteModelGroup refused to remove it because it now
// read as an auto-route. AutoOwner is the only marker that distinguishes a
// claimed auto-route from a hand-rolled one, so it gates the verb.
func TestReleaseModelGroupOwner_RefusesUnclaimedGroup(t *testing.T) {
	r := mountModelGroupsTestRouter(t, userAuthoredTestYAML)

	req := httptest.NewRequest("DELETE", "/zzrouter/v1/model-groups/prod-chat/owner", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())

	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Errors)
	assert.Equal(t, "route_not_claimed", body.Errors[0].Code,
		"agents branch on the closed-enum code, not the prose")

	// The group must be untouched — still deletable, still not auto.
	getReq := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/prod-chat", nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)
	var got struct {
		Data struct {
			AutoManaged bool   `json:"auto_managed"`
			AutoOwner   string `json:"auto_owner"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &got))
	assert.False(t, got.Data.AutoManaged, "a refused release must not flip AutoManaged")
	assert.Equal(t, "", got.Data.AutoOwner)
}
