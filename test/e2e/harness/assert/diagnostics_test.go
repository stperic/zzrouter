package assert_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/test/e2e/harness"
	"github.com/stperic/zzrouter/test/e2e/harness/assert"
	_ "github.com/stperic/zzrouter/test/e2e/harness/inproc"
)

func TestDiagnosticsAutoDumpOnFailure(t *testing.T) {
	cluster := mustProvision(t)
	root := t.TempDir()

	// Use a mock TB so the inner "failure" doesn't propagate to the
	// real test. assert.Diagnostics's t.Cleanup runs when we call
	// mock.runCleanups().
	mock := newMockTB("inner_fails")
	d := assert.Diagnostics(mock, cluster)
	if d == nil {
		t.Fatal("Diagnostics returned nil")
	}
	d.WithRoot(root)
	d.RecordRequest(&harness.RequestRecord{
		Method: "GET", URL: "http://example/foo",
	})
	d.RecordResponse(&harness.Response{
		Status:  500,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"error":"boom"}`),
	})
	mock.Fail()
	mock.runCleanups()

	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one runID dir under root, got entries=%v err=%v", entries, err)
	}
	runDir := filepath.Join(root, entries[0].Name())
	dumps, err := os.ReadDir(runDir)
	if err != nil || len(dumps) != 1 {
		t.Fatalf("expected one test dump under %s, got %v err=%v", runDir, dumps, err)
	}

	dest := filepath.Join(runDir, dumps[0].Name())
	for _, want := range []string{"request.curl", "response.json", "meta.json"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	if !strings.Contains(dumps[0].Name(), "inner_fails") {
		t.Errorf("dump dir %q should mention inner_fails", dumps[0].Name())
	}
}

func TestDiagnosticsNoDumpOnPass(t *testing.T) {
	cluster := mustProvision(t)
	root := t.TempDir()

	mock := newMockTB("inner_passes")
	d := assert.Diagnostics(mock, cluster)
	d.WithRoot(root)
	d.RecordResponse(&harness.Response{Status: 200})
	// no Fail — cleanup should NOT dump
	mock.runCleanups()

	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("expected empty root after pass, got %d entries", len(entries))
	}
}

func TestDiagnosticsNilClusterReturnsNil(t *testing.T) {
	if got := assert.Diagnostics(t, nil); got != nil {
		t.Errorf("Diagnostics(t, nil) = %v, want nil", got)
	}
}

// mockTB satisfies testing.TB by embedding it (the package-private
// method on testing.TB blocks external implementations otherwise).
// We override the methods Diagnostics-on-failure actually calls.
// Fatalf panics — fine because the auto-dump cleanup path only
// reaches Logf, never Fatalf/Errorf. Update this comment if that
// invariant changes.
type mockTB struct {
	testing.TB
	name     string
	failed   bool
	cleanups []func()
}

func newMockTB(name string) *mockTB { return &mockTB{name: name} }

func (m *mockTB) Helper()                           {}
func (m *mockTB) Name() string                      { return m.name }
func (m *mockTB) Failed() bool                      { return m.failed }
func (m *mockTB) Fail()                             { m.failed = true }
func (m *mockTB) Cleanup(f func())                  { m.cleanups = append(m.cleanups, f) }
func (m *mockTB) Logf(format string, args ...any)   {}
func (m *mockTB) Errorf(format string, args ...any) { m.failed = true }
func (m *mockTB) Fatalf(format string, args ...any) { m.failed = true; panic("fatal in mock TB") }
func (m *mockTB) runCleanups() {
	for i := len(m.cleanups) - 1; i >= 0; i-- {
		m.cleanups[i]()
	}
	m.cleanups = nil
}

func mustProvision(t *testing.T) *harness.Cluster {
	t.Helper()
	cfg := &harness.Config{
		Backend: harness.BackendInproc,
		Cluster: harness.ClusterTopology{
			Coordinator: harness.NodeSpec{Name: "coord", Role: harness.RoleCoordinator},
		},
	}
	cluster, err := harness.New(cfg)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cluster.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })
	return cluster
}
