package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ephemeralBaseYAML = `
version: "1"
model_groups:
  perpetual:
    strategy: priority
    replicas:
      - name: r1
        model: m
        provider: ollama
        priority: 1
`

func TestPUT_TTLSecondsSetsExpiresAtAndHeader(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	body := `{"strategy":"priority","ttl_seconds":600,"owner":"swarm-7","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	req := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/ephemeral", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	hdr := w.Header().Get("X-zzrouter-Expires-At")
	require.NotEmpty(t, hdr, "ephemeral PUT must emit X-zzrouter-Expires-At")
	parsed, err := time.Parse(time.RFC3339, hdr)
	require.NoError(t, err)
	// 600s ± slack — clock skew + test latency
	want := time.Now().UTC().Add(600 * time.Second)
	assert.InDelta(t, want.Unix(), parsed.Unix(), 5)

	// Verify body carries owner + expires_at.
	var got struct {
		Data struct {
			Owner     string `json:"owner"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "swarm-7", got.Data.Owner)
	assert.Equal(t, hdr, got.Data.ExpiresAt, "header and body field must match")
}

func TestPUT_NoTTLEmitsNoHeader(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	body := `{"strategy":"priority","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	req := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/perpetual", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("X-zzrouter-Expires-At"))
}

func TestPUT_TTLOutOfRange(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	for _, ttl := range []int{1, 59, 86401, 999999} {
		body := `{"strategy":"priority","ttl_seconds":` + ttlItoa(ttl) + `,"replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
		req := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/eph", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "ttl=%d should be rejected", ttl)
		assert.Contains(t, w.Body.String(), "out_of_range")
	}
}

func TestPUT_OwnerTooLong(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)
	owner := strings.Repeat("x", 129)
	body := `{"strategy":"priority","owner":"` + owner + `","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	req := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/eph", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "out_of_range")
}

func TestPATCH_TTLSecondsExtendsFromNow(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	// Seed an ephemeral via PUT with a short TTL.
	put := `{"strategy":"priority","ttl_seconds":120,"replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	pReq := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/extending", bytes.NewBufferString(put))
	pReq.Header.Set("Content-Type", "application/json")
	pW := httptest.NewRecorder()
	r.ServeHTTP(pW, pReq)
	require.Equal(t, http.StatusOK, pW.Code)

	// PATCH to 600s.
	patch := `{"ttl_seconds":600}`
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/extending", bytes.NewBufferString(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	hdr := w.Header().Get("X-zzrouter-Expires-At")
	require.NotEmpty(t, hdr)
	parsed, err := time.Parse(time.RFC3339, hdr)
	require.NoError(t, err)
	want := time.Now().UTC().Add(600 * time.Second)
	assert.InDelta(t, want.Unix(), parsed.Unix(), 5)
}

func TestPATCH_TTLSecondsNullClears(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	put := `{"strategy":"priority","ttl_seconds":600,"replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	pReq := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/clearing", bytes.NewBufferString(put))
	pReq.Header.Set("Content-Type", "application/json")
	pW := httptest.NewRecorder()
	r.ServeHTTP(pW, pReq)
	require.Equal(t, http.StatusOK, pW.Code)

	patch := `{"ttl_seconds":null}`
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/clearing", bytes.NewBufferString(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("X-zzrouter-Expires-At"),
		"null ttl_seconds clears ExpiresAt → no header")

	var got struct {
		Data struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Empty(t, got.Data.ExpiresAt)
}

func TestPATCH_OwnerNullClears(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)

	put := `{"strategy":"priority","owner":"swarm","replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	pReq := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/owned", bytes.NewBufferString(put))
	pReq.Header.Set("Content-Type", "application/json")
	pW := httptest.NewRecorder()
	r.ServeHTTP(pW, pReq)
	require.Equal(t, http.StatusOK, pW.Code)

	patch := `{"owner":null}`
	req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/owned", bytes.NewBufferString(patch))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var got struct {
		Data struct {
			Owner string `json:"owner"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Empty(t, got.Data.Owner)
}

func TestPATCH_TTLOutOfRange(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)
	for _, ttl := range []string{"30", "0", "86401"} {
		patch := `{"ttl_seconds":` + ttl + `}`
		req := httptest.NewRequest("PATCH", "/zzrouter/v1/model-groups/perpetual", bytes.NewBufferString(patch))
		req.Header.Set("Content-Type", "application/merge-patch+json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "ttl=%s should be rejected", ttl)
		assert.Contains(t, w.Body.String(), "out_of_range")
	}
}

func TestGET_ExpiresAtHeaderEcho(t *testing.T) {
	r := mountModelGroupsTestRouter(t, ephemeralBaseYAML)
	// Create ephemeral.
	put := `{"strategy":"priority","ttl_seconds":600,"replicas":[{"name":"r1","model":"m","provider":"ollama"}]}`
	pReq := httptest.NewRequest("PUT", "/zzrouter/v1/model-groups/eph", bytes.NewBufferString(put))
	pReq.Header.Set("Content-Type", "application/json")
	pW := httptest.NewRecorder()
	r.ServeHTTP(pW, pReq)
	require.Equal(t, http.StatusOK, pW.Code)

	getReq := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/eph", nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.NotEmpty(t, getW.Header().Get("X-zzrouter-Expires-At"))
}

func ttlItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
