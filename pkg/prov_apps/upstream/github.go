package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// latestGitHubRelease resolves the tag GitHub marks as the latest release.
//
// It reads the redirect from github.com/<repo>/releases/latest rather than
// calling the REST API. The API allows 60 unauthenticated requests per hour
// per IP, which a cluster checking several providers can plausibly exhaust,
// and this path costs none of that budget. The tradeoff is that publication
// date and release notes are not available; adding them means adding the API
// and a token to go with it.
//
// GitHub excludes prereleases from the release it marks latest, so the tag
// this returns is a stable release.
func (r *Resolver) latestGitHubRelease(ctx context.Context, src *config.VersionSource) (Release, error) {
	pageURL := fmt.Sprintf("%s/%s/releases/latest", r.githubBase, src.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, pageURL, nil)
	if err != nil {
		return Release{}, err
	}

	resp, err := r.noRedirect.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("upstream: %s: %w", src.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return Release{}, fmt.Errorf("upstream: %s: %w (HTTP %d)", src.Repo, ErrNoUpstreamRelease, resp.StatusCode)
	}

	// The redirect must land on this repo's own tag path. A repo that has
	// published no release at all redirects to the bare .../releases listing,
	// and taking the last path segment there yields the literal string
	// "releases" — a bogus install target and a bogus "update available".
	//
	// Location may be absolute or relative, so resolve it against the request
	// URL rather than matching the raw header. Checking host and path
	// separately also means a redirect off to another host or another repo
	// cannot contribute a tag, which matters because the tag flows into a
	// download URL downstream.
	locURL, err := url.Parse(loc)
	if err != nil {
		return Release{}, fmt.Errorf("upstream: %s returned an unparseable redirect %q: %w", src.Repo, loc, err)
	}
	resolved := resp.Request.URL.ResolveReference(locURL)
	if resolved.Host != resp.Request.URL.Host {
		return Release{}, fmt.Errorf("upstream: %s redirected off-host to %q", src.Repo, resolved.Host)
	}

	tag, ok := strings.CutPrefix(resolved.Path, fmt.Sprintf("/%s/releases/tag/", src.Repo))
	if !ok {
		return Release{}, fmt.Errorf("upstream: %s: %w (redirected to %q)", src.Repo, ErrNoUpstreamRelease, resolved.Path)
	}
	// A tag is a single path segment.
	if tag == "" || strings.Contains(tag, "/") {
		return Release{}, fmt.Errorf("upstream: %s returned an unusable tag in redirect %q", src.Repo, loc)
	}

	// GitHub's "latest" is whatever the project last marked as a full
	// release, which is not necessarily on the line this source tracks.
	// A tag the comparator cannot order is a non-answer: it renders as
	// status unknown forever, so no upgrade is ever offered. Fall back to
	// the release list and take the newest tag that CAN be ordered.
	if !orderable(src, tag) {
		return r.newestOrderableGitHubRelease(ctx, src, tag)
	}

	return Release{
		Version: src.Normalize(tag),
		Tag:     tag,
		URL:     fmt.Sprintf("%s/%s/releases/tag/%s", r.githubBase, src.Repo, tag),
	}, nil
}

// releaseListPageSize is how many releases one lookup considers. GitHub
// caps per_page at 100, and llama.cpp -- the reason this path exists --
// cuts several builds a day, so 100 covers roughly a fortnight. A line
// whose newest orderable tag has fallen off that window reports no
// release rather than an old one, which is the honest failure.
const releaseListPageSize = 100

// newestOrderableGitHubRelease resolves the newest release whose tag the
// source's comparator can order, used when the tag GitHub marks latest
// is not one of them.
//
// This is not hypothetical. Since 2026-08 llama.cpp cuts a versioned
// release (v0.2.0) carrying no binary bundle, and marks the build tags
// its binaries actually ship with (b10582) as prereleases. GitHub
// therefore reports v0.2.0 as latest, "v0.2.0" is not a build number,
// and llama.cpp went permanently uncomparable: status unknown on every
// node, update_available false, no upgrade ever offered.
//
// Cost: one call against the unauthenticated REST budget (60/hour per
// IP), which the redirect above deliberately avoids. It is spent only
// when the cheap answer is unusable, and the cache in front of this
// package is what keeps even that affordable.
//
// Prereleases are included on purpose. A project that publishes its
// build stream as prereleases makes that flag a statement about its
// release process, not about whether the artifact works. Orderability
// under the declared comparator is the filter; stability is not.
func (r *Resolver) newestOrderableGitHubRelease(ctx context.Context, src *config.VersionSource, latestTag string) (Release, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", r.githubAPIBase, src.Repo, releaseListPageSize)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("upstream: %s: %w", src.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 403 here is nearly always the rate limit. Naming the status
		// saves the next reader from diagnosing it as an auth problem.
		return Release{}, fmt.Errorf("upstream: %s: releases list returned HTTP %d", src.Repo, resp.StatusCode)
	}

	var payload []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("upstream: %s: releases list did not decode: %w", src.Repo, err)
	}

	candidates := make([]string, 0, len(payload))
	for _, rel := range payload {
		// A draft is visible only to the repo's maintainers and carries
		// no downloadable assets, so offering one as an upgrade target
		// would resolve to a 404.
		if rel.Draft || rel.TagName == "" {
			continue
		}
		candidates = append(candidates, rel.TagName)
	}

	// Newest orders by the comparator and skips what it cannot order, so
	// it does both jobs here: it drops the off-line tags and refuses to
	// let list position decide. GitHub sorts by publication date, and a
	// backported build published late would otherwise outrank a higher
	// build number.
	newest := Newest(src, candidates)
	if newest == "" {
		return Release{}, fmt.Errorf("upstream: %s: %w (GitHub reports %q as latest, which is not a %s identifier, and none of the newest %d releases is either)",
			src.Repo, ErrNoUpstreamRelease, latestTag, src.Comparator(), releaseListPageSize)
	}

	// Newest returns the raw tag it was given, so normalization for
	// display happens here -- the same split the redirect path makes.
	return Release{
		Version: src.Normalize(newest),
		Tag:     newest,
		URL:     fmt.Sprintf("%s/%s/releases/tag/%s", r.githubBase, src.Repo, newest),
	}, nil
}
