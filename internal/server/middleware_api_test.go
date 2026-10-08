package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRejectAPIKeyMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantDetail string
	}{
		{"no key → pass through", "", http.StatusOK, ""},
		{"any key → 400 clean error", "sk-abc", http.StatusBadRequest, "do not accept X-API-Key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(RequestIDMiddleware(), RejectAPIKeyMiddleware())
			r.GET("/ok", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

			req := httptest.NewRequest(http.MethodGet, "/ok", nil)
			if tc.header != "" {
				req.Header.Set("X-API-Key", tc.header)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantDetail != "" {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				detail, _ := body["detail"].(string)
				if !strings.Contains(detail, tc.wantDetail) {
					t.Errorf("detail = %q, want contains %q", detail, tc.wantDetail)
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
					t.Errorf("Content-Type = %q, want application/problem+json", ct)
				}
			}
		})
	}
}
