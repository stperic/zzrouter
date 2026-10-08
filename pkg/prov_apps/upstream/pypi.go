package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// simpleAPIAccept selects the PEP 691 JSON rendering of the simple index.
// Without it PyPI serves HTML.
const simpleAPIAccept = "application/vnd.pypi.simple.v1+json"

// maxSimpleIndexBytes caps the response body. The largest index in play is
// vllm's at roughly 166 KB across 95 versions, so this leaves ample headroom
// while still bounding memory if the endpoint misbehaves.
const maxSimpleIndexBytes = 16 << 20

// simpleIndex is the subset of PEP 691 / PEP 700 this package reads.
type simpleIndex struct {
	Versions []string `json:"versions"`
	Files    []struct {
		Filename string `json:"filename"`
		// Yanked is false, true, or a string reason, so it decodes as any.
		Yanked any `json:"yanked"`
	} `json:"files"`
}

// latestPyPI resolves the newest non-yanked version published for a package.
//
// It reads the simple index rather than the JSON API's info.version because
// the simple index exposes yank state per file, and yanks are not
// hypothetical: vllm currently carries five yanked files, one of which
// accounts for every file of version 0.2.1.
//
// What this returns is the latest *published* version. Whether it can be
// installed on a given node depends on that node's interpreter and platform,
// which the simple index cannot answer and this package does not pretend to.
func (r *Resolver) latestPyPI(ctx context.Context, src *config.VersionSource) (Release, error) {
	return r.latestPyPIMatching(ctx, src, "")
}

func (r *Resolver) latestPyPIMatching(ctx context.Context, src *config.VersionSource, constraint string) (Release, error) {
	indexURL := fmt.Sprintf("%s/simple/%s/", r.pypiBase, src.Package)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", simpleAPIAccept)

	resp, err := r.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("upstream: %s: %w", src.Package, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("upstream: %s simple index returned HTTP %d", src.Package, resp.StatusCode)
	}

	var index simpleIndex
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSimpleIndexBytes)).Decode(&index); err != nil {
		return Release{}, fmt.Errorf("upstream: %s simple index is unreadable: %w", src.Package, err)
	}

	candidates := index.Versions
	if len(candidates) == 0 {
		return Release{}, fmt.Errorf("upstream: %s simple index lists no versions: %w", src.Package, ErrNoUpstreamRelease)
	}

	live := withoutFullyYanked(candidates, index)
	filtered := live[:0]
	for _, candidate := range live {
		if VersionAllowed(candidate, constraint) == nil {
			filtered = append(filtered, candidate)
		}
	}
	live = filtered
	newest := Newest(src, live)
	if newest == "" {
		return Release{}, fmt.Errorf("upstream: %s has no orderable released version: %w", src.Package, ErrNoUpstreamRelease)
	}

	return Release{
		Version: src.Normalize(newest),
		Tag:     newest,
		URL:     fmt.Sprintf("%s/project/%s/%s/", r.pypiBase, src.Package, newest),
	}, nil
}

// withoutFullyYanked drops versions whose every file is yanked. A version
// with at least one live file stays: yanking a single bad wheel does not
// retract the release.
func withoutFullyYanked(versions []string, index simpleIndex) []string {
	live := make(map[string]bool, len(versions))
	for _, f := range index.Files {
		v := versionFromFilename(f.Filename)
		if v == "" {
			continue
		}
		if !isYanked(f.Yanked) {
			live[v] = true
		} else if _, seen := live[v]; !seen {
			live[v] = false
		}
	}

	kept := make([]string, 0, len(versions))
	for _, v := range versions {
		// A version with no recognizable files is kept rather than dropped:
		// an unparsed filename is our gap, not upstream retracting a release.
		if ok, found := live[v]; found && !ok {
			continue
		}
		kept = append(kept, v)
	}
	return kept
}

// isYanked reads PEP 592's tri-state: absent or false means live, true or a
// string reason means yanked.
func isYanked(v any) bool {
	switch y := v.(type) {
	case bool:
		return y
	case string:
		return true
	default:
		return false
	}
}

// versionFromFilename extracts the version from a distribution filename.
//
// Wheels are {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
// and PEP 427 escapes any hyphen in the distribution name to an underscore,
// so the second hyphen-separated field is the version. Source distributions
// are {name}-{version}{ext}, and PEP 440 versions contain no hyphen, so the
// final hyphen separates the two.
//
// Verified against vllm's full index: every filename resolved to a version
// the index also lists, with no leftovers.
func versionFromFilename(name string) string {
	if strings.HasSuffix(name, ".whl") {
		parts := strings.Split(strings.TrimSuffix(name, ".whl"), "-")
		if len(parts) < 2 {
			return ""
		}
		return parts[1]
	}
	for _, ext := range []string{".tar.gz", ".zip", ".tar.bz2"} {
		if !strings.HasSuffix(name, ext) {
			continue
		}
		base := strings.TrimSuffix(name, ext)
		if i := strings.LastIndex(base, "-"); i > 0 {
			return base[i+1:]
		}
		return ""
	}
	return ""
}
