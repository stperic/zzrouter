package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Root probes preserve Ollama handshakes and explicit JSON identity.
// Browsers receive the same identity when the console is disabled.
func TestRootProbe_ContentNegotiation(t *testing.T) {
	cases := []struct {
		name       string
		accept     string
		wantJSON   bool
		wantString string
	}{
		{name: "no accept header", accept: "", wantString: ollamaRootProbeBody},
		{name: "wildcard keeps the ollama handshake", accept: "*/*", wantString: ollamaRootProbeBody},
		{name: "text/plain", accept: "text/plain", wantString: ollamaRootProbeBody},
		{name: "explicit json", accept: "application/json", wantJSON: true},
		{name: "json among several", accept: "application/json, text/plain;q=0.9", wantJSON: true},
		{name: "json after plain", accept: "text/plain, application/json", wantJSON: true},
		{name: "json after wildcard", accept: "*/*, application/json", wantJSON: true},
		{name: "html without console", accept: "text/html", wantJSON: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			c.Request = req

			s := &Server{access: &AccessControl{}}
			s.handleRoot(c)

			if w.Header().Get("Vary") != "Accept" || w.Header().Get("Cache-Control") != "no-store" {
				t.Error("negotiated root must vary by Accept and prevent caching")
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", w.Code)
			}
			if !tc.wantJSON {
				if got := w.Body.String(); got != tc.wantString {
					t.Errorf("body: got %q, want %q", got, tc.wantString)
				}
				return
			}
			var identity struct {
				Service string            `json:"service"`
				Links   map[string]string `json:"links"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &identity); err != nil {
				t.Fatalf("identity document is not JSON: %v", err)
			}
			if identity.Service != "zzrouter" {
				t.Errorf("service: got %q, want %q", identity.Service, "zzrouter")
			}
			for _, key := range []string{"health", "discovery", "openapi"} {
				if identity.Links[key] == "" {
					t.Errorf("identity document is missing the %q link", key)
				}
			}
		})
	}
}
