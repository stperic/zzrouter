package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
)

// ProviderVersionsService answers "is a newer release of this provider
// available upstream?".
//
// It reports; it never upgrades. Two comparisons are reported separately
// because they call for different actions: an installed version behind
// upstream means upgrade that node, while a pin behind upstream means every
// node added next will start stale.
type ProviderVersionsService struct {
	appsConfig func() *pkgConfig.AppsConfig
	cache      upstreamCache
}

// upstreamCache is the slice of *upstream.Cache this service needs. Declared
// here, at the consumer, so tests can substitute a fake without the resolver
// growing a test-only exported seam.
type upstreamCache interface {
	// Lookup may make a network call; Peek never does. Both key on an
	// upstream.CacheKey, which is the provider plus the platform when the
	// provider resolves to a different source per platform.
	Lookup(ctx context.Context, key string, src *pkgConfig.VersionSource, refresh bool) upstream.Result
	Peek(key string) (upstream.Result, bool)
}

// NewProviderVersionsService wires the service to the coordinator's live
// provider config. Checks run coordinator-side: workers do not each reach out.
func NewProviderVersionsService(appsConfig func() *pkgConfig.AppsConfig, cache upstreamCache) *ProviderVersionsService {
	return &ProviderVersionsService{appsConfig: appsConfig, cache: cache}
}

// nodeInstall is one node's installed version of a provider, plus where that
// node runs. The OS decides which upstream governs it.
type nodeInstall struct {
	Node    string
	Version string
	OS      string
}

// nodeVersionInfo is one node's installed version and how it compares.
type nodeVersionInfo struct {
	Node      string `json:"node"`
	Installed string `json:"installed,omitempty"`
	// Status orders Installed against the newest upstream release:
	// "newer" means upstream is ahead, "older" means this node is, "same"
	// means they match, "unknown" means no ordering was established.
	Status string `json:"status"`
	// UpdateAvailable restates Status == "newer" without requiring the
	// reader to know which operand the comparison names. It is false
	// whenever the answer is unknown, never optimistically true.
	UpdateAvailable bool `json:"update_available"`
	// Reason explains an unknown status, and is absent otherwise.
	Reason string `json:"reason,omitempty"`
	// InstallableLatest is the newest release THIS node can actually
	// install, and InstallableSource names the upstream that caps it. Both
	// are present only when that differs from the report's headline, which
	// is what the project published rather than what this node can reach.
	//
	// Deliberately not called "latest": the headline field of that name
	// answers a different question, and a caller choosing a version to send
	// to /upgrade must not have to infer which one it is holding.
	InstallableLatest string `json:"installable_latest,omitempty"`
	InstallableSource string `json:"installable_source,omitempty"`
}

// versionSourceInfo echoes the configured feed so a caller can see where the
// answer came from without reading provider config.
type versionSourceInfo struct {
	Type    string `json:"type"`
	Repo    string `json:"repo,omitempty"`
	Package string `json:"package,omitempty"`
}

// ProviderVersionsReport is the response body for the versions endpoint.
type ProviderVersionsReport struct {
	Provider string `json:"provider"`
	// Latest is the newest published version, normalized for display.
	Latest string `json:"latest,omitempty"`
	// LatestTag is the raw upstream identifier ("b10502" where Latest is
	// "10502"). Either field works as an install/upgrade version: each
	// installer coerces the value to the shape its own download URLs need.
	LatestTag  string             `json:"latest_tag,omitempty"`
	ReleaseURL string             `json:"release_url,omitempty"`
	CheckedAt  string             `json:"checked_at,omitempty"`
	Source     *versionSourceInfo `json:"source,omitempty"`

	// Pinned is the install-time seed, and PinnedStatus compares it to
	// Latest. A pin behind upstream is about future installs, not this node.
	Pinned       string `json:"pinned,omitempty"`
	PinnedStatus string `json:"pinned_status"`

	// UpdateAvailable is true when any node is behind upstream. It answers
	// the question a caller actually has ("is there anything to do here?")
	// without making it reduce a per-node status list first.
	UpdateAvailable bool `json:"update_available"`
	// UpgradeURL is the endpoint that acts on that answer. Present only
	// when there is something to upgrade.
	UpgradeURL string `json:"upgrade_url,omitempty"`

	// Nodes carries the per-node installed comparison.
	Nodes []nodeVersionInfo `json:"nodes"`

	// Reason explains an unknown result: not tracked, checks disabled, or the
	// lookup error. Present only when Latest is empty.
	Reason string `json:"reason,omitempty"`
}

// checksEnabled reports whether outbound lookups are permitted at all.
func (s *ProviderVersionsService) checksEnabled() bool {
	cfg := s.appsConfig()
	if cfg == nil {
		return false
	}
	return cfg.Settings.Updates.ProviderVersionChecksEnabled()
}

// Report builds the versions report for one provider. installed carries what
// each node has, and where that node runs.
func (s *ProviderVersionsService) Report(ctx context.Context, provider string, installed []nodeInstall, refresh bool) (*ProviderVersionsReport, error) {
	cfg := s.appsConfig()
	if cfg == nil {
		return nil, fmt.Errorf("provider config unavailable")
	}

	report := &ProviderVersionsReport{
		Provider:     provider,
		PinnedStatus: string(upstream.StatusUnknown),
		Nodes:        s.nodesFrom(ctx, provider, installed, nil, nil, false),
	}
	if svc, ok := cfg.LookupApp(provider); ok {
		report.Pinned = svc.PinnedVersion
	}

	src, err := cfg.VersionSourceFor(provider)
	if err != nil {
		// A provider nobody tracks is a reportable state, not a failure:
		// the caller still wants the installed versions it already has.
		if errors.Is(err, pkgConfig.ErrNoVersionSource) {
			report.Reason = "provider declares no version_source"
			return report, nil
		}
		return nil, err
	}
	report.Source = &versionSourceInfo{Type: string(src.Type), Repo: src.Repo, Package: src.Package}

	if !s.checksEnabled() {
		report.Reason = "upstream version checks are disabled in settings"
		return report, nil
	}

	res := s.cache.Lookup(ctx, provider, src, refresh)
	if !res.CheckedAt.IsZero() {
		report.CheckedAt = res.CheckedAt.UTC().Format(time.RFC3339)
	}
	if res.Err != nil {
		// Deliberately not an HTTP error: an unreachable upstream leaves the
		// installed versions perfectly reportable, and the status stays
		// unknown rather than degrading to "same". The lookup failure is
		// carried in Reason instead.
		report.Reason = res.Err.Error()
		return report, nil //nolint:nilerr // an upstream outage is a reportable state, not a request failure
	}

	report.Latest = res.Release.Version
	report.LatestTag = res.Release.Tag
	report.ReleaseURL = res.Release.URL
	report.PinnedStatus = string(upstream.Compare(src, report.Pinned, res.Release.Version))
	// Seed with the headline's own result so the nodes on the base source
	// reuse the fetch that has already happened rather than repeating it.
	report.Nodes = s.nodesFrom(ctx, provider, installed, src,
		map[string]upstream.Result{upstream.CacheKey(provider, "", src): res}, refresh)
	for _, n := range report.Nodes {
		if n.UpdateAvailable {
			report.UpdateAvailable = true
			report.UpgradeURL = "/zzrouter/v1/providers/" + provider + "/upgrade"
			break
		}
	}

	return report, nil
}

// DecorateApps fills the cached upstream fields on a provider list. It is
// cache-only and never makes a network call, so listing providers cannot
// block on an upstream.
func (s *ProviderVersionsService) DecorateApps(apps []AppInfo) {
	cfg := s.appsConfig()
	if cfg == nil || !s.checksEnabled() {
		return
	}

	for i := range apps {
		base, err := cfg.VersionSourceFor(apps[i].Name)
		if err != nil {
			continue
		}
		// The source governing THIS row's node. A provider installed through
		// a packager on one platform is capped by that packager there, and
		// annotating the row with the project's own release would offer an
		// upgrade the node cannot perform.
		src := base.ForOS(apps[i].OS)
		key := upstream.CacheKey(apps[i].Name, apps[i].OS, base)
		got, ok := s.cache.Peek(key)
		if !ok {
			// Cold or stale: populate in the background so the next render
			// can annotate. Without this the only thing that ever warms a
			// provider is opening its detail pane, so the list annotates
			// whichever providers happen to have been visited and silently
			// omits the rest.
			s.warm(key, src)
			continue
		}
		if got.Err != nil {
			// A cached failure: leave the row bare rather than claiming
			// currency, and do not re-fetch until the negative TTL expires.
			continue
		}
		// The shape that pairs with THIS row's installed version, so a
		// client can render the two side by side without knowing which
		// form the provider's installer records.
		apps[i].LatestVersion = upstream.DisplayVersion(apps[i].Version, got.Release)
		status := upstream.Compare(src, apps[i].Version, got.Release.Version)
		apps[i].VersionStatus = string(status)
		apps[i].UpdateAvailable = status == upstream.StatusNewer
		apps[i].VersionCheckedAt = got.CheckedAt.UTC().Format(time.RFC3339)
	}
}

// warmTimeout bounds a background cache fill. Shorter than the resolver's own
// ceiling because nobody is waiting on it: if it does not land quickly, the
// next list render will try again.
const warmTimeout = 20 * time.Second

// warm populates one provider's cache entry off the request path.
//
// The goroutine is bounded by warmTimeout and the cache collapses concurrent
// fills for the same provider through singleflight, so repeated list requests
// against a cold cache produce one outbound call rather than one per request.
func (s *ProviderVersionsService) warm(key string, src *pkgConfig.VersionSource) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()
		s.cache.Lookup(ctx, key, src, false)
	}()
}

// nodesFrom builds the per-node comparison. With no resolved release every
// node reports unknown, which is what an offline or untracked provider must
// look like.
func (s *ProviderVersionsService) nodesFrom(
	ctx context.Context, provider string, installed []nodeInstall,
	base *pkgConfig.VersionSource, resolved map[string]upstream.Result, refresh bool,
) []nodeVersionInfo {
	// Resolve each distinct upstream at most once. Nodes usually share a
	// source, and refresh=true makes every Lookup an outbound request, so
	// looking up per node turns one refresh into one request per node.
	// resolved arrives carrying the headline's own result, which keeps the
	// common case at zero extra fetches.
	if resolved == nil {
		resolved = make(map[string]upstream.Result, 2)
	}
	lookup := func(key string, src *pkgConfig.VersionSource) upstream.Result {
		if got, ok := resolved[key]; ok {
			return got
		}
		got := s.cache.Lookup(ctx, key, src, refresh)
		resolved[key] = got
		return got
	}

	out := make([]nodeVersionInfo, 0, len(installed))
	for _, n := range installed {
		info := nodeVersionInfo{Node: n.Node, Installed: n.Version, Status: string(upstream.StatusUnknown)}
		if base == nil {
			// The top-level Reason already carries why there is no upstream
			// answer; repeating it per node would be noise.
			info.Reason = "no upstream release to compare against"
			out = append(out, info)
			continue
		}

		// The source governing THIS node, which is not always the one the
		// report's top-level Latest came from: a provider installed through
		// a packager on one platform has that packager's ceiling there.
		src := base.ForOS(n.OS)
		res := lookup(upstream.CacheKey(provider, n.OS, base), src)
		switch {
		case res.Err != nil:
			// Say why. The top-level Reason covers the headline's own
			// lookup, and this node may answer to a different upstream that
			// failed on its own — the only place that cause is reportable.
			info.Reason = res.Err.Error()
			out = append(out, info)
			continue
		case res.Release.Version == "":
			info.Reason = "no upstream release to compare against"
			out = append(out, info)
			continue
		}

		status := upstream.Compare(src, n.Version, res.Release.Version)
		info.Status = string(status)
		info.UpdateAvailable = status == upstream.StatusNewer
		info.Reason = upstream.Explain(src, n.Version, res.Release.Version)
		// Reported per node only when this node answers to a different
		// upstream than the report's headline, so the common case stays
		// uncluttered and the exception is impossible to miss.
		if src != base {
			info.InstallableLatest = upstream.DisplayVersion(n.Version, res.Release)
			info.InstallableSource = string(src.Type)
		}
		out = append(out, info)
	}
	// Stable order so a list response does not reshuffle between requests.
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// upstreamCheckInterval reads the configured cache TTL, falling back to the
// package default when settings are absent or unparseable.
func upstreamCheckInterval(cfg *pkgConfig.AppsConfig) time.Duration {
	if cfg == nil {
		return upstream.DefaultTTL
	}
	return cfg.Settings.Updates.CheckIntervalOrDefault(upstream.DefaultTTL)
}
