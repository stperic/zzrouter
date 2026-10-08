package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileByName_Unknown(t *testing.T) {
	if _, err := profileByName("nonsense"); err == nil {
		t.Fatal("expected error on unknown profile")
	}
}

func TestProfile_BaseDefaults(t *testing.T) {
	c := Base()
	c.applyDefaults()
	if c.Backend != BackendInproc {
		t.Errorf("Backend default: got %v want %v", c.Backend, BackendInproc)
	}
	if c.Cassettes.Mode != CloudReplay {
		t.Errorf("Cassettes.Mode default: got %v want %v", c.Cassettes.Mode, CloudReplay)
	}
	if c.Timeouts.JobTerminal == 0 {
		t.Error("JobTerminal default not applied")
	}
}

func TestValidate_RequiredEnv(t *testing.T) {
	c := Base()
	c.RequiredEnv = []string{"HARNESS_TEST_REQUIRED_ENV_X"}
	os.Unsetenv("HARNESS_TEST_REQUIRED_ENV_X")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "HARNESS_TEST_REQUIRED_ENV_X") {
		t.Fatalf("want missing-env error, got %v", err)
	}
	t.Setenv("HARNESS_TEST_REQUIRED_ENV_X", "1")
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected validate error with env set: %v", err)
	}
}

func TestValidate_InstallOnUnknownNode(t *testing.T) {
	c := Base()
	c.RequiredEnv = nil
	c.Providers = []ProviderSpec{{Name: "vllm", InstallOn: []string{"ghost"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown node") {
		t.Fatalf("want unknown-node error, got %v", err)
	}
}

func TestValidate_TwoPrimariesPerRole(t *testing.T) {
	c := Base()
	c.RequiredEnv = nil
	c.Models = []ModelSpec{
		{ID: "a", Role: RoleChat, Primary: true},
		{ID: "b", Role: RoleChat, Primary: true},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "two primary models") {
		t.Fatalf("want duplicate-primary error, got %v", err)
	}
}

func TestPrimaryModel_NotFound(t *testing.T) {
	c := Base()
	if _, err := c.PrimaryModel(RoleChat); err == nil {
		t.Fatal("expected error when no primary registered")
	}
}

func TestProvidersForNode_TagFilter(t *testing.T) {
	c := Base()
	c.Cluster.Workers = []NodeSpec{
		{Name: "gpu-box", Role: RoleWorker, Tags: []string{"gpu", "cuda"}},
		{Name: "cpu-box", Role: RoleWorker, Tags: []string{"cpu"}},
	}
	c.Providers = []ProviderSpec{
		{Name: "vllm", InstallOn: []string{"gpu-box", "cpu-box"}, RequiresTags: []string{"gpu"}},
		{Name: "llamacpp", InstallOn: []string{"gpu-box", "cpu-box"}},
	}

	got := c.ProvidersForNode("cpu-box")
	if len(got) != 1 || got[0].Name != "llamacpp" {
		t.Errorf("cpu-box should only get llamacpp, got %v", got)
	}
	got = c.ProvidersForNode("gpu-box")
	if len(got) != 2 {
		t.Errorf("gpu-box should get both, got %v", got)
	}
}

func TestProvidersForNode_DisabledOverlay(t *testing.T) {
	c := Base()
	c.Cluster.Workers = []NodeSpec{{Name: "n", Role: RoleWorker}}
	c.Providers = []ProviderSpec{{Name: "mlx", InstallOn: []string{"n"}}}
	c.ProvidersDisabled = []string{"mlx"}
	if got := c.ProvidersForNode("n"); len(got) != 0 {
		t.Errorf("disabled provider leaked through: %v", got)
	}
}

func TestApplyOverlay(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "site.yaml")
	body := `cluster:
  coordinator:
    address: 10.99.99.1
    api_port: 19090
  workers:
    - name: w1
      address: 10.99.99.2
      role: worker
      tags: [gpu]
providers_disabled: [mlx]
`
	if err := os.WriteFile(overlay, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")

	cfg, err := Load("local", overlay)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cluster.Coordinator.Address != "10.99.99.1" {
		t.Errorf("overlay address not applied: %s", cfg.Cluster.Coordinator.Address)
	}
	if cfg.Cluster.Coordinator.APIPort != 19090 {
		t.Errorf("overlay port not applied: %d", cfg.Cluster.Coordinator.APIPort)
	}
	if len(cfg.ProvidersDisabled) != 1 || cfg.ProvidersDisabled[0] != "mlx" {
		t.Errorf("providers_disabled not applied: %v", cfg.ProvidersDisabled)
	}
}

// fixtureRouteResponse is a minimal but realistic /server/routes payload.
const fixtureRouteResponse = `{
  "count": 3,
  "routes": [
    {"method":"GET","path":"/health","surface":"health","auth":"none"},
    {"method":"POST","path":"/zzrouter/v1/runs","surface":"admin","auth":"admin","async":true},
    {"method":"GET","path":"/v1/chat/completions","surface":"openai","auth":"optional","streaming":"sse"}
  ]
}`

func TestFetchRoutes_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/server/routes" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "test-cluster-key" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureRouteResponse))
	}))
	defer srv.Close()

	cat, err := FetchRoutes(context.Background(), srv.Client(), srv.URL, "test-cluster-key")
	if err != nil {
		t.Fatalf("FetchRoutes: %v", err)
	}
	if cat.Count != 3 || len(cat.Routes) != 3 {
		t.Fatalf("unexpected catalog: count=%d len=%d", cat.Count, len(cat.Routes))
	}
}

func TestFetchRoutes_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchRoutes(context.Background(), srv.Client(), srv.URL, "bad"); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestFetchRoutes_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 99, "routes": [{"method":"GET","path":"/x","surface":"other","auth":"none"}]}`))
	}))
	defer srv.Close()
	_, err := FetchRoutes(context.Background(), srv.Client(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "count mismatch") {
		t.Fatalf("want count-mismatch error, got %v", err)
	}
}

func TestApplyOverlay_WorkersListReplaces(t *testing.T) {
	// Operator-surprise guard: yaml.v3 replaces slices when the key is
	// present. Documenting the contract here so it can't silently flip.
	dir := t.TempDir()
	overlay := filepath.Join(dir, "site.yaml")
	body := `cluster:
  workers:
    - name: only-worker
      address: 10.0.0.99
      role: worker
`
	if err := os.WriteFile(overlay, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")

	cfg, err := Load("local", overlay) // local has 1 worker
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Cluster.Workers) != 1 || cfg.Cluster.Workers[0].Name != "only-worker" {
		t.Fatalf("expected workers list replaced, got %+v", cfg.Cluster.Workers)
	}
}

func TestApplyOverlay_RejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "site.yaml")
	body := "workrs: [oops]\n" // typo
	if err := os.WriteFile(overlay, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")
	if _, err := Load("local", overlay); err == nil || !strings.Contains(err.Error(), "workrs") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

// TestApplyOverlay_RelativePathResolvesAgainstRepoRoot pins the
// CWD-tolerant overlay loader: a relative path that fails against
// CWD must be retried against the nearest go.mod parent. Bug
// repro: env ZZROUTER_E2E_SITES=test/e2e/configs/sites/example.yaml +
// `go test ./test/e2e/ring2_admin/` resolved against the package
// dir instead of the repo root and surfaced as a confusing
// "open ...: no such file or directory" before propagating to a
// nil-cluster panic in every other test in the package.
func TestApplyOverlay_RelativePathResolvesAgainstRepoRoot(t *testing.T) {
	// Plant a sentinel overlay under a synthetic repo root and chdir
	// into a child dir so the loader has to walk up to find go.mod.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	overlayBody := "cluster:\n  coordinator:\n    address: 192.0.2.1\n"
	if err := os.WriteFile(filepath.Join(root, "configs", "site.yaml"), []byte(overlayBody), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "test", "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevCwd) })
	if err := os.Chdir(pkgDir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")

	// Relative path — does not exist under pkgDir, must be resolved
	// against root.
	cfg, err := Load("local", "configs/site.yaml")
	if err != nil {
		t.Fatalf("Load with relative path: %v", err)
	}
	if cfg.Cluster.Coordinator.Address != "192.0.2.1" {
		t.Errorf("overlay not applied: got %q want 192.0.2.1", cfg.Cluster.Coordinator.Address)
	}
}

func TestNodeSpec_IsEnabledTristate(t *testing.T) {
	n := NodeSpec{}
	if !n.IsEnabled() {
		t.Error("nil Enabled should default to enabled")
	}
	on := true
	off := false
	if n2 := (NodeSpec{Enabled: &off}); n2.IsEnabled() {
		t.Error("explicit false should disable")
	}
	if n3 := (NodeSpec{Enabled: &on}); !n3.IsEnabled() {
		t.Error("explicit true should enable")
	}
}

func TestProvidersForNode_DisabledNodeReturnsNil(t *testing.T) {
	c := Base()
	off := false
	c.Cluster.Workers = []NodeSpec{{Name: "w", Role: RoleWorker, Enabled: &off}}
	c.Providers = []ProviderSpec{{Name: "llamacpp", InstallOn: []string{"w"}}}
	if got := c.ProvidersForNode("w"); got != nil {
		t.Errorf("disabled worker should yield nil providers, got %v", got)
	}
}

func TestValidate_RejectsUnknownCassetteMode(t *testing.T) {
	c := Base()
	c.RequiredEnv = nil
	c.Cassettes.Mode = "bogus"
	c.applyDefaults() // should not overwrite explicit non-empty value
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cassettes.mode") {
		t.Fatalf("want cassette-mode error, got %v", err)
	}
}

func TestValidate_TwoCloudPrimariesPerRole(t *testing.T) {
	c := Base()
	c.RequiredEnv = nil
	c.Models = []ModelSpec{
		{ID: "a", Role: RoleChat, Source: "cloud", Primary: true},
		{ID: "b", Role: RoleChat, Source: "cloud", Primary: true},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "two primary models") {
		t.Fatalf("want duplicate-cloud-primary error, got %v", err)
	}
}

func TestValidate_LocalAndCloudPrimaryCoexist(t *testing.T) {
	c := Base()
	c.RequiredEnv = nil
	c.Models = []ModelSpec{
		{ID: "local", Role: RoleChat, Source: "huggingface://x/y", Primary: true},
		{ID: "cloud", Role: RoleChat, Source: "cloud", Primary: true},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("local+cloud primaries should coexist: %v", err)
	}
}

func TestProfile_NightlyAndFullCompose(t *testing.T) {
	for _, name := range []string{"nightly", "full"} {
		c, err := profileByName(name)
		if err != nil {
			t.Fatalf("profileByName(%q): %v", name, err)
		}
		c.applyDefaults()
		if c.Backend != BackendSSH {
			t.Errorf("%s: want BackendSSH got %v", name, c.Backend)
		}
		if len(c.Providers) == 0 || len(c.Models) == 0 {
			t.Errorf("%s: missing providers/models", name)
		}
	}
}

func TestPartitionRoutes(t *testing.T) {
	in := []RouteSpec{
		{Method: "GET", Path: "/a"},
		{Method: "POST", Path: "/b"},
		{Method: "DELETE", Path: "/c"},
		{Method: "GET", Path: "/d"},
	}
	ro, mu := PartitionRoutes(in)
	if len(ro) != 2 || len(mu) != 2 {
		t.Fatalf("partition: ro=%v mu=%v", ro, mu)
	}
}

func TestGroupBySurface(t *testing.T) {
	in := []RouteSpec{
		{Method: "GET", Path: "/a", Surface: "health"},
		{Method: "GET", Path: "/v1/x", Surface: "openai"},
		{Method: "GET", Path: "/v1/y", Surface: "openai"},
	}
	g := GroupBySurface(in)
	if len(g["openai"]) != 2 || len(g["health"]) != 1 {
		t.Fatalf("unexpected grouping: %v", g)
	}
}

func TestSSHProfileRequiresSiteOverlay(t *testing.T) {
	for _, profile := range []string{"nightly", "full"} {
		for _, sites := range [][]string{nil, {""}} {
			if _, err := Load(profile, sites...); err == nil || !strings.Contains(err.Error(), "requires a site overlay") {
				t.Fatalf("Load(%q, %v) = %v, want explicit overlay error", profile, sites, err)
			}
		}
	}
}
