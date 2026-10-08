package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validFingerprint is the canonical 64-char lowercase hex form the
// revoke handler expects to see on the wire after normalization.
const validFingerprint = "b48f003237148e99d3b2e7f5a0c41d8fe62e9b4d8c7a162b3d59e8f47c95a0aa"

// postRevoke drives POST /zzrouter/v1/cluster/revoke-worker with the
// given fingerprint string. Serial / reason are empty — the handler
// doesn't require them.
func postRevoke(t *testing.T, s *Server, fingerprint string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"fingerprint": fingerprint})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/cluster/revoke-worker", bytes.NewReader(body))
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("Content-Type", "application/json")
	s.engine.ServeHTTP(w, req)
	return w
}

func TestRevokeWorker_AcceptsCanonicalHex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := coordTestServer(t)

	w := postRevoke(t, s, validFingerprint)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out struct {
		Status      string `json:"status"`
		Fingerprint string `json:"fingerprint"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "revoked", out.Status)
	assert.Equal(t, validFingerprint, out.Fingerprint)
}

func TestRevokeWorker_AcceptsSHA256PrefixAndNormalizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := coordTestServer(t)

	// Uppercase + sha256: prefix. Handler must lowercase and strip
	// the prefix; response echoes the canonical form.
	w := postRevoke(t, s, "sha256:B48F003237148E99D3B2E7F5A0C41D8FE62E9B4D8C7A162B3D59E8F47C95A0AA")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, validFingerprint, out.Fingerprint,
		"response must echo the normalized (lowercase, no prefix) fingerprint")
}

// TestRevokeWorker_RejectsNonHexFingerprint pins the F2 fix: the
// handler must reject display-only forms, empty strings, non-hex
// garbage, and wrong-length inputs BEFORE they poison the deny list.
func TestRevokeWorker_RejectsNonHexFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		fp   string
	}{
		{"empty", ""},
		{"not hex", "not-a-hex-string"},
		// The ellipsis form from ShortForm output (U+2026), exactly
		// what a careless operator might paste. Length happens to be
		// short too, but the character class is what matters.
		{"ellipsis form", "b48f0032…95a0aa3a"},
		// Valid hex chars but wrong length.
		{"too short", "b48f0032"},
		{"too long", validFingerprint + "aa"},
		// Non-hex char in an otherwise 64-char string.
		{"non-hex char", "g" + validFingerprint[1:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := coordTestServer(t)
			w := postRevoke(t, s, tc.fp)
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"input %q must be rejected with 400, got %d body=%s",
				tc.fp, w.Code, w.Body.String())
			// Error body mentions "fingerprint" — either from the
			// binding:"required" tag (empty-string case) or from our
			// explicit normalization error (everything else).
			assert.Contains(t, w.Body.String(), "ingerprint",
				"error message must be operator-actionable")
		})
	}
}

// TestRevokeWorker_UppercaseIsLowercased pins that the normalizer
// lowercases — uppercase hex is legal input, but the deny list is
// written in canonical lowercase so paste-then-audit is consistent.
func TestRevokeWorker_UppercaseIsLowercased(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := coordTestServer(t)

	upper := "B48F003237148E99D3B2E7F5A0C41D8FE62E9B4D8C7A162B3D59E8F47C95A0AA"
	w := postRevoke(t, s, upper)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, validFingerprint, out.Fingerprint)
}
