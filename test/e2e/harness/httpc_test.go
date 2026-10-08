package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixtureNode wraps an httptest server in a Node so Client can dial it.
func fixtureNode(t *testing.T, h http.Handler) (*Node, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	n, err := NewNode("test", srv.URL, RoleCoordinator, []string{"test"},
		NodeKeys{Admin: "ADMIN", API: "READONLY", Cluster: "CLUSTER"})
	if err != nil {
		t.Fatal(err)
	}
	return n, srv
}

func TestClient_TierMatrix(t *testing.T) {
	cases := []struct {
		name string
		tier StaticTier
		want string
	}{
		{"none", TierNone, ""},
		{"admin", TierAdmin, "ADMIN"},
		{"api", TierAPI, "READONLY"},
		{"cluster", TierCluster, "CLUSTER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenKey string
			n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenKey = r.Header.Get("X-API-Key")
				w.WriteHeader(204)
			}))
			c := NewClient(n, tc.tier)
			if _, err := c.GET(context.Background(), "/x"); err != nil {
				t.Fatal(err)
			}
			if seenKey != tc.want {
				t.Errorf("X-API-Key: got %q want %q", seenKey, tc.want)
			}
		})
	}
}

func TestClient_RequestIDEcho(t *testing.T) {
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "rid-99")
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	resp, err := c.GET(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "rid-99" {
		t.Errorf("RequestID echo: got %q", resp.RequestID)
	}
}

func TestClient_VirtualKeyOverridesStatic(t *testing.T) {
	var seenKey string
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("X-API-Key")
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.GET(context.Background(), "/x", AsVirtualKey("vkey-123"))
	if err != nil {
		t.Fatal(err)
	}
	if seenKey != "vkey-123" {
		t.Errorf("expected virtual key, got %q", seenKey)
	}
}

func TestClient_POST_JSONBody(t *testing.T) {
	type In struct {
		Name string `json:"name"`
	}
	var got In
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "wrong ct", 400)
			return
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"job_id":"job-1"}`))
	}))
	c := NewClient(n, TierAdmin)
	resp, err := c.POST(context.Background(), "/zzrouter/v1/runs", In{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "x" {
		t.Errorf("body: got %+v", got)
	}
	id, err := resp.JobID()
	if err != nil || id != "job-1" {
		t.Errorf("JobID: id=%q err=%v", id, err)
	}
}

func TestClient_PATCH_MergePatch(t *testing.T) {
	var ct string
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.PATCH(context.Background(), "/x", map[string]any{"a": 1}, MergePatch())
	if err != nil {
		t.Fatal(err)
	}
	if ct != "application/merge-patch+json" {
		t.Errorf("Content-Type: got %q", ct)
	}
}

func TestClient_QueryAndIdempotency(t *testing.T) {
	var idem string
	var q string
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idem = r.Header.Get("Idempotency-Key")
		q = r.URL.RawQuery
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.GET(context.Background(), "/x",
		Query("foo", "bar"),
		Query("baz", "qux"),
		IdempotencyKey("abc-123"))
	if err != nil {
		t.Fatal(err)
	}
	if idem != "abc-123" {
		t.Errorf("Idempotency-Key: got %q", idem)
	}
	if !strings.Contains(q, "foo=bar") || !strings.Contains(q, "baz=qux") {
		t.Errorf("query: got %q", q)
	}
}

func TestResponse_Problem_RFC9457(t *testing.T) {
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"too many","status":429,"code":"rate_limited"}`))
	}))
	c := NewClient(n, TierNone)
	resp, err := c.GET(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	p, err := resp.Problem()
	if err != nil {
		t.Fatalf("Problem: %v", err)
	}
	if p.Code != "rate_limited" || p.Status != 429 {
		t.Errorf("problem: %+v", p)
	}
}

func TestResponse_Problem_NotProblemJSON(t *testing.T) {
	r := Response{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte("{}")}
	if _, err := r.Problem(); err == nil {
		t.Fatal("expected error on non-problem+json")
	}
}

func TestResponse_JobID_NotAccepted(t *testing.T) {
	r := Response{Status: 200, Body: []byte(`{"job_id":"x"}`)}
	if _, err := r.JobID(); err == nil {
		t.Fatal("expected error on non-202")
	}
}

func TestClient_RawBody_RequiresContentType(t *testing.T) {
	var sawCT string
	var gotBody []byte
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.POST(context.Background(), "/x", nil,
		RawBody("application/octet-stream", []byte{0xde, 0xad}))
	if err != nil {
		t.Fatal(err)
	}
	if sawCT != "application/octet-stream" {
		t.Errorf("RawBody CT not applied: %q", sawCT)
	}
	if len(gotBody) != 2 || gotBody[0] != 0xde {
		t.Errorf("raw body: %v", gotBody)
	}
}

func TestClient_WithHTTPClient_UsesOverride(t *testing.T) {
	var hits int
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	override := &http.Client{Transport: &http.Transport{}}
	c2 := c.WithHTTPClient(override)
	if _, err := c2.GET(context.Background(), "/x"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("override client did not reach fixture: hits=%d", hits)
	}
}

func TestClient_UserHeaderDoesNotDuplicateAuth(t *testing.T) {
	var keys []string
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = r.Header.Values("X-API-Key")
		w.WriteHeader(204)
	}))
	c := NewClient(n, TierAdmin)
	// User header for X-API-Key shouldn't multiply with auto-set admin key.
	_, err := c.GET(context.Background(), "/x", Header("X-API-Key", "user-supplied"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 X-API-Key header, got %v", keys)
	}
	// applyAuth runs after user headers and uses Set, so the static-tier
	// admin key wins. Document this in the API.
	if keys[0] != "ADMIN" {
		t.Errorf("expected admin key to win, got %q", keys[0])
	}
}

func TestClient_HEAD(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(200)
		// net/http strips body on HEAD; writing here is a no-op.
		_, _ = w.Write([]byte("ignored"))
	}))
	c := NewClient(n, TierAdmin)
	resp, err := c.HEAD(context.Background(), "/api/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodHead {
		t.Errorf("server saw method=%q, want HEAD", gotMethod)
	}
	if gotPath != "/api/blobs/sha256:abc" {
		t.Errorf("server saw path=%q", gotPath)
	}
	if resp.Status != 200 {
		t.Errorf("status=%d, want 200", resp.Status)
	}
	if resp.Headers.Get("X-Custom") != "yes" {
		t.Errorf("expected X-Custom header to round-trip")
	}
	if len(resp.Body) != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", len(resp.Body))
	}
}

func TestClient_POSTMultipart(t *testing.T) {
	var (
		gotCT       string
		gotPurpose  string
		gotFileName string
		gotFileMIME string
		gotFileBody []byte
	)
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		// 32 KiB plenty for tiny test payloads.
		if err := r.ParseMultipartForm(32 << 10); err != nil { //nolint:gosec // In-process test sends a bounded synthetic multipart body.
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(500)
			return
		}
		gotPurpose = r.FormValue("purpose") //nolint:gosec // In-process test sends a bounded synthetic multipart body.
		f, fh, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			w.WriteHeader(500)
			return
		}
		defer f.Close()
		gotFileName = fh.Filename
		gotFileMIME = fh.Header.Get("Content-Type")
		body, _ := io.ReadAll(f)
		gotFileBody = body
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"file-x"}`))
	}))
	c := NewClient(n, TierAdmin)

	resp, err := c.POSTMultipart(context.Background(), "/v1/files",
		map[string]string{"purpose": "fine-tune"},
		[]MultipartFile{{
			Field:    "file",
			Filename: "training.jsonl",
			Content:  []byte(`{"prompt":"hi","completion":"hello"}` + "\n"),
			MIME:     "application/jsonl",
		}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 201 {
		t.Errorf("status=%d, want 201", resp.Status)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Errorf("server Content-Type=%q, want multipart/form-data with boundary", gotCT)
	}
	if gotPurpose != "fine-tune" {
		t.Errorf("form purpose=%q, want fine-tune", gotPurpose)
	}
	if gotFileName != "training.jsonl" {
		t.Errorf("filename=%q, want training.jsonl", gotFileName)
	}
	if gotFileMIME != "application/jsonl" {
		t.Errorf("file MIME=%q, want application/jsonl", gotFileMIME)
	}
	if !bytes.Contains(gotFileBody, []byte(`"prompt":"hi"`)) {
		t.Errorf("file body did not round-trip: %q", gotFileBody)
	}
}

func TestClient_POSTMultipart_DefaultMIME(t *testing.T) {
	var gotMIME string
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(8 << 10) //nolint:gosec // In-process test sends a bounded synthetic multipart body.
		_, fh, err := r.FormFile("blob")
		if err != nil {
			w.WriteHeader(500)
			return
		}
		gotMIME = fh.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.POSTMultipart(context.Background(), "/anything", nil,
		[]MultipartFile{{Field: "blob", Filename: "x.bin", Content: []byte{1, 2, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotMIME != "application/octet-stream" {
		t.Errorf("default MIME=%q, want application/octet-stream", gotMIME)
	}
}

func TestClient_POSTMultipart_MissingField(t *testing.T) {
	n, _ := fixtureNode(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	c := NewClient(n, TierAdmin)
	_, err := c.POSTMultipart(context.Background(), "/x", nil,
		[]MultipartFile{{Filename: "x.bin", Content: []byte{1}}})
	if err == nil {
		t.Fatal("expected error for missing Field name")
	}
	if !strings.Contains(err.Error(), "Field name") {
		t.Errorf("err=%v, want one mentioning Field name", err)
	}
}
