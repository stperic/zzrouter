package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/test/e2e/harness"
	_ "github.com/stperic/zzrouter/test/e2e/harness/inproc"
)

func TestDiagnosticsDumpMinimal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	root := t.TempDir()
	d := harness.NewDiagnostics(cluster).WithRoot(root)

	dest, err := d.Dump(ctx, harness.Bundle{
		TestName: "TestDiagnosticsDumpMinimal",
		FailedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !strings.HasPrefix(dest, root) {
		t.Errorf("dest %q not under root %q", dest, root)
	}
	mustExist(t, filepath.Join(dest, "meta.json"))
}

func TestDiagnosticsDumpFull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	root := t.TempDir()
	d := harness.NewDiagnostics(cluster).WithRoot(root)

	failedAt := time.Now().UTC()
	dest, err := d.Dump(ctx, harness.Bundle{
		TestName: "test/with slashes & spaces",
		FailedAt: failedAt,
		GitSHA:   "deadbeef",
		Request: &harness.RequestRecord{
			Method:  "POST",
			URL:     "http://example/foo",
			Headers: http.Header{"X-API-Key": []string{"secret"}, "Content-Type": []string{"application/json"}},
			Body:    []byte(`{"k":"v"}`),
		},
		Response: &harness.Response{
			Status:    500,
			Headers:   http.Header{"Content-Type": []string{"application/json"}},
			Body:      []byte(`{"error":"boom"}`),
			RequestID: "req-123",
		},
		JobsSSE: map[string][]byte{
			"job-abc": []byte("event: done\ndata: {}\n\n"),
		},
		StateTar: []byte{0x1f, 0x8b, 0x00, 0x00},
		Extra:    map[string]any{"profile": "local"},
	})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	// Sanitized test-name path component — no slashes leak.
	rel, _ := filepath.Rel(root, dest)
	parts := strings.Split(rel, string(filepath.Separator))
	for _, p := range parts {
		if strings.ContainsAny(p, "/ &") {
			t.Errorf("path component %q not sanitized", p)
		}
	}

	mustExist(t, filepath.Join(dest, "request.curl"))
	mustExist(t, filepath.Join(dest, "response.json"))
	mustExist(t, filepath.Join(dest, "meta.json"))
	mustExist(t, filepath.Join(dest, "state-snapshot.tar.gz"))
	mustExist(t, filepath.Join(dest, "jobs", "job-abc.sse"))

	// Curl line is single-line + contains the body verbatim.
	curl, err := os.ReadFile(filepath.Join(dest, "request.curl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(curl), "\n") > 1 {
		t.Errorf("curl line should be single-line, got: %q", curl)
	}
	if !strings.Contains(string(curl), `{"k":"v"}`) {
		t.Errorf("curl missing body, got: %q", curl)
	}

	// response.json round-trips JSON body inline (not base64).
	respBytes, _ := os.ReadFile(filepath.Join(dest, "response.json"))
	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("response.json invalid: %v", err)
	}
	if resp["status"].(float64) != 500 {
		t.Errorf("status got %v want 500", resp["status"])
	}
	if _, isB64 := resp["body_b64"]; isB64 {
		t.Error("valid JSON body should be inline, not body_b64")
	}

	// meta.json carries Extra + diagnostics fields.
	metaBytes, _ := os.ReadFile(filepath.Join(dest, "meta.json"))
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("meta.json invalid: %v", err)
	}
	if meta["profile"] != "local" {
		t.Error("Extra not merged into meta.json")
	}
	if meta["backend"] != "inproc" {
		t.Errorf("backend got %v want inproc", meta["backend"])
	}
	if meta["git_sha"] != "deadbeef" {
		t.Errorf("git_sha got %v want deadbeef", meta["git_sha"])
	}
}

func TestDiagnosticsRequiresTestName(t *testing.T) {
	cluster := mustProvisionInproc(t, context.Background())
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	d := harness.NewDiagnostics(cluster).WithRoot(t.TempDir())
	if _, err := d.Dump(context.Background(), harness.Bundle{}); err == nil {
		t.Fatal("Dump with empty TestName should error, got nil")
	}
}

func TestDiagnosticsLogFetcherErrorRecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	root := t.TempDir()
	d := harness.NewDiagnostics(cluster).WithRoot(root)
	d.SetLogFetcher(cluster.Coordinator().Name(), errFetcher{})

	dest, err := d.Dump(ctx, harness.Bundle{TestName: "fetch-error"})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	metaBytes, _ := os.ReadFile(filepath.Join(dest, "meta.json"))
	var meta map[string]any
	_ = json.Unmarshal(metaBytes, &meta)
	notes, _ := meta["diagnostics"].([]any)
	found := false
	for _, n := range notes {
		if s, _ := n.(string); strings.Contains(s, "boom") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fetch error not recorded in diagnostics notes: %v", notes)
	}
}

func TestDiagnosticsJobsSSEManifest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	root := t.TempDir()
	d := harness.NewDiagnostics(cluster).WithRoot(root)

	// Two job IDs that sanitize to the SAME filename — exercises the
	// collision-suffix path. "job/abc" and "job_abc" both sanitize
	// to "job_abc".
	dest, err := d.Dump(ctx, harness.Bundle{
		TestName: "TestDiagnosticsJobsSSEManifest",
		FailedAt: time.Now().UTC(),
		JobsSSE: map[string][]byte{
			"job/abc": []byte("a"),
			"job_abc": []byte("b"),
		},
	})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(dest, "jobs", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest) != 2 {
		t.Errorf("manifest should map 2 entries, got %d: %v", len(manifest), manifest)
	}
	// Both originals must appear in the manifest values, even though
	// their sanitized basenames collide.
	values := map[string]bool{}
	for _, v := range manifest {
		values[v] = true
	}
	if !values["job/abc"] || !values["job_abc"] {
		t.Errorf("manifest values missing originals: %v", manifest)
	}
}

func TestToTarball(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, err := harness.ToTarball(dir)
	if err != nil {
		t.Fatalf("ToTarball: %v", err)
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("not gzipped (magic %x %x)", body[0], body[1])
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected file %s: %v", p, err)
	}
}

type errFetcher struct{}

func (errFetcher) Name() string { return "err" }
func (errFetcher) FetchLogs(context.Context, *harness.Node, time.Time, time.Duration) ([]byte, error) {
	return nil, errors.New("boom")
}
