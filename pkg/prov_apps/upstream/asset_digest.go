package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// releaseBodyMaxBytes bounds the release document read. A llama.cpp release
// lists ~25 assets in roughly 40 KB; 4 MB leaves three orders of magnitude of
// headroom while still refusing to buffer an unbounded response.
const releaseBodyMaxBytes = 4 << 20

// digestPrefix is how GitHub qualifies the hash algorithm in an asset digest
// ("sha256:<hex>"). Only SHA-256 is published today, and VerifySHA256 is what
// consumes the result, so a digest under any other algorithm is reported as
// absent rather than silently compared with the wrong hash.
const digestPrefix = "sha256:"

// AssetDigest reports the SHA-256 GitHub recorded for one asset of a release,
// as lowercase hex. It returns "" (with a nil error) when the release exists
// but publishes no SHA-256 digest for that filename.
//
// This is the only GitHub REST API call in the package. latestGitHubRelease
// deliberately avoids the API to stay clear of its 60-requests-per-hour
// unauthenticated budget, which a cluster polling several providers could
// plausibly exhaust. The arithmetic differs here: a digest is fetched once per
// artifact per install, not once per provider per poll.
//
// It exists because not every project publishes a checksum file. llama.cpp
// publishes none at all, so the SHA256SUMS URL derived from its release path
// has always 404'd — while GitHub itself records a digest for every asset.
func AssetDigest(ctx context.Context, src *config.VersionSource, tag, filename string) (string, error) {
	return defaultResolver.AssetDigest(ctx, src, tag, filename)
}

// AssetDigest reports the SHA-256 GitHub recorded for one asset of a release.
func (r *Resolver) AssetDigest(ctx context.Context, src *config.VersionSource, tag, filename string) (string, error) {
	// Validate for the same reason Latest does: Repo is interpolated into the
	// URL below without escaping, and config.VersionSource.Validate is what
	// constrains it to "owner/name". Every caller today comes through
	// ProviderVersionSource, which already validates — this keeps the next
	// one from being the exception.
	if err := src.Validate(); err != nil {
		return "", err
	}
	if src.Repo == "" {
		return "", fmt.Errorf("upstream: asset digest needs a repo")
	}
	if tag == "" {
		return "", fmt.Errorf("upstream: %s: asset digest needs a release tag", src.Repo)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases/tags/%s",
		r.githubAPIBase, src.Repo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upstream: %s: %w", src.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream: %s release %s: HTTP %d", src.Repo, tag, resp.StatusCode)
	}

	var doc struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, releaseBodyMaxBytes))
	if err != nil {
		return "", fmt.Errorf("upstream: %s release %s: %w", src.Repo, tag, err)
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("upstream: %s release %s: %w", src.Repo, tag, err)
	}

	for _, a := range doc.Assets {
		if a.Name != filename {
			continue
		}
		hex, ok := strings.CutPrefix(a.Digest, digestPrefix)
		if !ok {
			return "", nil
		}
		return strings.ToLower(hex), nil
	}
	return "", nil
}
