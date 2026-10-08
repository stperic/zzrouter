package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
)

// fakeCache stands in for *upstream.Cache. It records whether a lookup was
// attempted, which is how the tests assert that the list path and the
// checks-disabled path make no outbound call.
type fakeCache struct {
	mu       sync.Mutex
	result   upstream.Result
	lookups  int
	warm     bool
	lastFlag bool
}

func (f *fakeCache) Lookup(_ context.Context, _ string, _ *pkgConfig.VersionSource, refresh bool) upstream.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	f.lastFlag = refresh
	f.warm = true
	return f.result
}

func (f *fakeCache) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeCache) Peek(string) (upstream.Result, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.warm {
		return upstream.Result{}, false
	}
	return f.result, true
}

func okResult(tag, version string) upstream.Result {
	return upstream.Result{
		Release:   upstream.Release{Tag: tag, Version: version, URL: "https://example.invalid/" + tag},
		CheckedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}
}

// versionsFixture builds a config carrying one tracked provider plus a stub
// upstream, and returns a service wired to both.
func versionsFixture(t *testing.T, cache *fakeCache, checksEnabled bool) *ProviderVersionsService {
	t.Helper()

	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("llamacpp", pkgConfig.ServiceConfig{
		Protocol:      pkgConfig.ProtocolOpenAI,
		Mode:          "on-demand",
		PinnedVersion: "b10453",
		VersionSource: &pkgConfig.VersionSource{
			Type:        pkgConfig.VersionSourceGitHubRelease,
			Repo:        "ggml-org/llama.cpp",
			StripPrefix: "b",
			Compare:     pkgConfig.CompareBuildNumber,
		},
		Runtime: &pkgConfig.AppRuntimeConfig{
			BasePort:  8080,
			PortRange: []int{8080, 8089},
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	// An untracked provider: present in config, no version_source.
	require.NoError(t, cfg.AddApp("custom", pkgConfig.ServiceConfig{
		Protocol:     pkgConfig.ProtocolOpenAI,
		Mode:         "on-demand",
		Runtime:      &pkgConfig.AppRuntimeConfig{BasePort: 9100, PortRange: []int{9100, 9109}, Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "x"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))

	enabled := checksEnabled
	cfg.Settings.Updates.ProviderVersionChecks = &enabled

	return NewProviderVersionsService(func() *pkgConfig.AppsConfig { return cfg }, cache)
}

func TestProviderVersionsReport(t *testing.T) {
	svc := versionsFixture(t, &fakeCache{result: okResult("b10502", "10502")}, true)

	report, err := svc.Report(context.Background(), "llamacpp", []nodeInstall{
		{Node: "coord", Version: "b10453"},
		{Node: "worker-1", Version: "b10502"},
		{Node: "stale", Version: "b9000"},
	}, false)
	require.NoError(t, err)

	assert.Equal(t, "10502", report.Latest)
	// The raw tag, because that is what an install request needs.
	assert.Equal(t, "b10502", report.LatestTag)
	assert.Equal(t, "b10453", report.Pinned)
	assert.Equal(t, "newer", report.PinnedStatus, "the pin seeds new installs and is behind")
	assert.NotEmpty(t, report.CheckedAt)
	require.NotNil(t, report.Source)
	assert.Equal(t, "github_release", report.Source.Type)

	// Per-node, because the action differs per node.
	byNode := map[string]string{}
	for _, n := range report.Nodes {
		byNode[n.Node] = n.Status
	}
	assert.Equal(t, "newer", byNode["coord"])
	assert.Equal(t, "same", byNode["worker-1"])
	assert.Equal(t, "newer", byNode["stale"])
}

// An unreachable upstream must not fail the request, and must never render as
// "up to date".
func TestProviderVersionsReportSurvivesUpstreamFailure(t *testing.T) {
	svc := versionsFixture(t, &fakeCache{result: upstream.Result{
		Err:       errors.New("dial tcp: connection refused"),
		CheckedAt: time.Now(),
	}}, true)

	report, err := svc.Report(context.Background(), "llamacpp", []nodeInstall{{Node: "coord", Version: "b10453"}}, false)
	require.NoError(t, err, "an upstream outage is reportable, not an HTTP error")

	assert.Empty(t, report.Latest)
	assert.Equal(t, "unknown", report.PinnedStatus)
	assert.NotEmpty(t, report.Reason)
	require.Len(t, report.Nodes, 1)
	assert.Equal(t, "unknown", report.Nodes[0].Status)
	assert.Equal(t, "b10453", report.Nodes[0].Installed, "installed versions stay reportable")
}

func TestProviderVersionsReportUntrackedProvider(t *testing.T) {
	svc := versionsFixture(t, &fakeCache{result: okResult("b10502", "10502")}, true)

	report, err := svc.Report(context.Background(), "custom", []nodeInstall{{Node: "coord", Version: "1.0"}}, false)
	require.NoError(t, err)
	assert.Empty(t, report.Latest)
	assert.Contains(t, report.Reason, "no version_source")
	assert.Equal(t, "unknown", report.PinnedStatus)
	require.Len(t, report.Nodes, 1)
	assert.Equal(t, "unknown", report.Nodes[0].Status)
}

// An air-gapped cluster must make no outbound request at all.
func TestProviderVersionsChecksDisabled(t *testing.T) {
	cache := &fakeCache{result: okResult("b10502", "10502")}
	svc := versionsFixture(t, cache, false)

	report, err := svc.Report(context.Background(), "llamacpp", []nodeInstall{{Node: "coord", Version: "b10453"}}, false)
	require.NoError(t, err)
	assert.Zero(t, cache.lookupCount(), "checks disabled must make no upstream request")
	assert.Empty(t, report.Latest)
	assert.Contains(t, report.Reason, "disabled")
	assert.Equal(t, "unknown", report.PinnedStatus)
}

func TestProviderVersionsUnknownProvider(t *testing.T) {
	svc := versionsFixture(t, &fakeCache{result: okResult("b10502", "10502")}, true)
	_, err := svc.Report(context.Background(), "nope", nil, false)
	require.Error(t, err)
}

// The list path never annotates from a synchronous fetch: a cold cache leaves
// the fields empty for this render rather than blocking the response on an
// upstream call. Filling happens in the background; see the warming test.
func TestDecorateAppsIsCacheOnly(t *testing.T) {
	cache := &fakeCache{result: okResult("b10502", "10502")}
	svc := versionsFixture(t, cache, true)

	apps := []AppInfo{{Name: "llamacpp", Node: "coord", Version: "b10453"}}
	svc.DecorateApps(apps)

	assert.Empty(t, apps[0].LatestVersion, "a cold cache must not annotate synchronously")
	assert.Empty(t, apps[0].VersionStatus)

	// Once the entry exists, decorating annotates without any further fetch.
	require.Eventually(t, func() bool { return cache.lookupCount() >= 1 },
		2*time.Second, 10*time.Millisecond)
	before := cache.lookupCount()

	svc.DecorateApps(apps)
	assert.Equal(t, before, cache.lookupCount(), "a warm entry must not refetch")
	assert.Equal(t, "b10502", apps[0].LatestVersion, "display uses the raw tag so it pairs with the pin")
	assert.Equal(t, "newer", apps[0].VersionStatus)
	assert.NotEmpty(t, apps[0].VersionCheckedAt)
}

// A cold cache must fill itself off the request path. Without this the only
// thing that ever warms a provider is opening its detail pane, so the list
// annotates whichever providers were visited and silently omits the rest.
func TestDecorateAppsWarmsAColdCacheInBackground(t *testing.T) {
	cache := &fakeCache{result: okResult("b10502", "10502")}
	svc := versionsFixture(t, cache, true)

	apps := []AppInfo{{Name: "llamacpp", Node: "coord", Version: "b10453"}}
	svc.DecorateApps(apps)

	// The call itself stays cache-only: nothing is annotated on this pass.
	assert.Empty(t, apps[0].LatestVersion, "the first render must not block on a fetch")

	// ...but a fill was kicked off, so the next render has data.
	require.Eventually(t, func() bool { return cache.lookupCount() == 1 },
		2*time.Second, 10*time.Millisecond, "a cold entry must be warmed in the background")

	svc.DecorateApps(apps)
	assert.Equal(t, "b10502", apps[0].LatestVersion)
	assert.Equal(t, "newer", apps[0].VersionStatus)
}

// A cached failure must not be retried on every list render, or an
// unreachable upstream turns each refresh into a fresh outbound attempt.
func TestDecorateAppsDoesNotRetryACachedFailure(t *testing.T) {
	cache := &fakeCache{result: upstream.Result{Err: errors.New("boom"), CheckedAt: time.Now()}}
	cache.warm = true
	svc := versionsFixture(t, cache, true)

	apps := []AppInfo{{Name: "llamacpp", Node: "coord", Version: "b10453"}}
	for range 3 {
		svc.DecorateApps(apps)
	}

	assert.Zero(t, cache.lookupCount(), "a cached failure must not trigger a refetch")
	assert.Empty(t, apps[0].VersionStatus, "a failed check must not annotate the row")
}

// keyedCache answers per cache key, which is what a provider with a
// per-platform source needs: one ceiling for the Mac, another for Linux.
type keyedCache struct {
	mu      sync.Mutex
	results map[string]upstream.Result
	keys    []string
}

func (k *keyedCache) Lookup(_ context.Context, key string, _ *pkgConfig.VersionSource, _ bool) upstream.Result {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
	return k.results[key]
}

func (k *keyedCache) Peek(key string) (upstream.Result, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	got, ok := k.results[key]
	return got, ok
}

// ollamaSplitFixture mirrors the shipped ollama config: GitHub releases
// everywhere, Homebrew on macOS, because that is what a Mac can install.
func ollamaSplitFixture(t *testing.T, cache upstreamCache) *ProviderVersionsService {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", pkgConfig.ServiceConfig{
		Protocol: pkgConfig.ProtocolOllama,
		Mode:     "external",
		VersionSource: &pkgConfig.VersionSource{
			Type:        pkgConfig.VersionSourceGitHubRelease,
			Repo:        "ollama/ollama",
			StripPrefix: "v",
			Compare:     pkgConfig.CompareSemver,
			Platforms: map[string]*pkgConfig.VersionSource{
				"darwin": {Type: pkgConfig.VersionSourceHomebrew, Formula: "ollama", Compare: pkgConfig.CompareSemver},
			},
		},
		Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: "http://localhost:11434"},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	enabled := true
	cfg.Settings.Updates.ProviderVersionChecks = &enabled
	return NewProviderVersionsService(func() *pkgConfig.AppsConfig { return cfg }, cache)
}

// The bug this fixes: a Mac on 0.32.14 was told 0.32.15 was available,
// because the check read GitHub while the Mac installs from Homebrew, whose
// formula had not caught up. Pressing upgrade could not succeed.
func TestReportComparesEachNodeAgainstItsOwnUpstream(t *testing.T) {
	cache := &keyedCache{results: map[string]upstream.Result{
		"ollama":           okResult("v0.32.15", "0.32.15"), // GitHub
		"ollama\x00darwin": okResult("0.32.14", "0.32.14"),  // Homebrew formula
	}}
	svc := ollamaSplitFixture(t, cache)

	report, err := svc.Report(context.Background(), "ollama", []nodeInstall{
		{Node: "macbook-pro", Version: "0.32.14", OS: "darwin"},
		{Node: "worker-1", Version: "0.21.2", OS: "linux"},
	}, false)
	require.NoError(t, err)

	byNode := map[string]nodeVersionInfo{}
	for _, n := range report.Nodes {
		byNode[n.Node] = n
	}

	mac := byNode["macbook-pro"]
	assert.Equal(t, "same", mac.Status, "the Mac is at Homebrew's ceiling, so there is nothing to install")
	assert.False(t, mac.UpdateAvailable)
	assert.Equal(t, "0.32.14", mac.InstallableLatest, "the node's own ceiling is named when it differs")
	assert.Equal(t, "homebrew", mac.InstallableSource)

	linux := byNode["worker-1"]
	assert.Equal(t, "newer", linux.Status, "Linux installs the GitHub release and is genuinely behind")
	assert.True(t, linux.UpdateAvailable)
	assert.Empty(t, linux.InstallableSource, "a node on the headline source needs no annotation")

	// The headline stays the project's own newest release: that is the
	// honest answer to "what has upstream published".
	assert.Equal(t, "0.32.15", report.Latest)
	assert.True(t, report.UpdateAvailable, "some node can still act")
}

// A node whose OS is unknown gets the base source rather than a guess:
// comparing against the wrong ceiling is worse than comparing against the
// project's own release.
func TestReportFallsBackToBaseSourceForUnknownOS(t *testing.T) {
	cache := &keyedCache{results: map[string]upstream.Result{
		"ollama": okResult("v0.32.15", "0.32.15"),
	}}
	svc := ollamaSplitFixture(t, cache)

	report, err := svc.Report(context.Background(), "ollama",
		[]nodeInstall{{Node: "mystery", Version: "0.32.14"}}, false)
	require.NoError(t, err)

	require.Len(t, report.Nodes, 1)
	assert.Equal(t, "newer", report.Nodes[0].Status)
	assert.Empty(t, report.Nodes[0].InstallableSource)
}

// The list path must annotate each row against its own node's ceiling too,
// or the providers table contradicts the detail pane.
func TestDecorateAppsUsesThePerNodeSource(t *testing.T) {
	cache := &keyedCache{results: map[string]upstream.Result{
		"ollama":           okResult("v0.32.15", "0.32.15"),
		"ollama\x00darwin": okResult("0.32.14", "0.32.14"),
	}}
	svc := ollamaSplitFixture(t, cache)

	apps := []AppInfo{
		{Name: "ollama", Node: "macbook-pro", Version: "0.32.14", OS: "darwin"},
		{Name: "ollama", Node: "worker-1", Version: "0.21.2", OS: "linux"},
	}
	svc.DecorateApps(apps)

	assert.Equal(t, "same", apps[0].VersionStatus)
	assert.False(t, apps[0].UpdateAvailable)
	assert.Equal(t, "0.32.14", apps[0].LatestVersion)

	assert.Equal(t, "newer", apps[1].VersionStatus)
	assert.True(t, apps[1].UpdateAvailable)
	assert.Equal(t, "0.32.15", apps[1].LatestVersion)
}

// countingCache records how many outbound lookups a report costs.
type countingCache struct {
	mu      sync.Mutex
	results map[string]upstream.Result
	calls   map[string]int
}

func (c *countingCache) Lookup(_ context.Context, key string, _ *pkgConfig.VersionSource, _ bool) upstream.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[key]++
	return c.results[key]
}

func (c *countingCache) Peek(key string) (upstream.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	got, ok := c.results[key]
	return got, ok
}

// refresh=true bypasses the cache, so resolving per node would turn one
// refresh into one outbound request per node, against the same feed.
func TestReportResolvesEachUpstreamOncePerCall(t *testing.T) {
	cache := &countingCache{
		results: map[string]upstream.Result{
			"ollama":           okResult("v0.32.15", "0.32.15"),
			"ollama\x00darwin": okResult("0.32.14", "0.32.14"),
		},
		calls: map[string]int{},
	}
	svc := ollamaSplitFixture(t, cache)

	_, err := svc.Report(context.Background(), "ollama", []nodeInstall{
		{Node: "macbook-pro", Version: "0.32.14", OS: "darwin"},
		{Node: "linux-a", Version: "0.21.2", OS: "linux"},
		{Node: "linux-b", Version: "0.30.0", OS: "linux"},
		{Node: "linux-c", Version: "0.31.0", OS: "linux"},
	}, true)
	require.NoError(t, err)

	// One for the headline, reused by all three Linux nodes; one for the
	// only node whose ceiling genuinely differs.
	assert.Equal(t, 1, cache.calls["ollama"], "three Linux nodes share one feed")
	assert.Equal(t, 1, cache.calls["ollama\x00darwin"])
}
