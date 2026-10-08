package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

const (
	// GitHubAPIBase is the base URL for GitHub API
	GitHubAPIBase = "https://api.github.com"

	// DefaultOwner is the default GitHub repository owner
	DefaultOwner = "stperic"

	// DefaultRepo is the default GitHub repository name
	DefaultRepo = "zzrouter"

	// DefaultTimeout is the default HTTP timeout for API requests
	DefaultTimeout = 30 * time.Second

	// maxAPIResponseSize is the maximum allowed size for GitHub API responses (10MB)
	maxAPIResponseSize = 10 * 1024 * 1024
)

// RateLimitInfo holds GitHub API rate limit information
type RateLimitInfo struct {
	Limit     int       // Total requests allowed
	Remaining int       // Requests remaining
	Reset     time.Time // When the limit resets
}

// Checker handles checking for updates from GitHub Releases
type Checker struct {
	httpClient   *http.Client
	owner        string
	repo         string
	channel      string
	pinnedVer    string
	assetPattern *regexp.Regexp // Pre-compiled pattern for asset matching
	apiBaseURL   string         // Configurable API base URL (for testing)
	rateLimit    *RateLimitInfo // Last known rate limit info
}

// NewChecker creates a new update checker
func NewChecker(channel, pinnedVersion string) *Checker {
	c := &Checker{
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		owner:      DefaultOwner,
		repo:       DefaultRepo,
		channel:    channel,
		pinnedVer:  pinnedVersion,
		apiBaseURL: GitHubAPIBase,
	}
	c.compileAssetPattern()
	return c
}

// SetAPIBaseURL sets a custom API base URL (for testing)
func (c *Checker) SetAPIBaseURL(baseURL string) {
	c.apiBaseURL = baseURL
}

// Check checks for available updates
func (c *Checker) Check(ctx context.Context) (*CheckResult, error) {
	currentVersion := version.Current

	result := &CheckResult{
		CurrentVersion:  currentVersion,
		UpdateAvailable: false,
	}

	// Fetch releases from GitHub
	releases, err := c.fetchReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}

	if len(releases) == 0 {
		result.SkippedReason = "no releases found"
		return result, nil
	}

	// Find the latest applicable release
	latestRelease := c.findLatestApplicableRelease(releases)
	if latestRelease == nil {
		result.SkippedReason = "no applicable release found for channel and platform"
		return result, nil
	}

	result.LatestRelease = latestRelease

	// Validate release has a valid version
	if latestRelease.Version == nil {
		result.SkippedReason = "latest release has no valid version"
		return result, nil
	}

	// Check if version is pinned
	if c.pinnedVer != "" {
		if !c.matchesVersionPin(latestRelease.Version, c.pinnedVer) {
			result.SkippedReason = fmt.Sprintf("version %s does not match pin %s", latestRelease.Version.String(), c.pinnedVer)
			return result, nil
		}
		result.MatchesPin = true
	}

	// Compare versions
	if latestRelease.Version.IsGreaterThan(currentVersion) {
		result.UpdateAvailable = true
	} else {
		result.SkippedReason = "already at latest version"
	}

	return result, nil
}

// fetchReleases fetches all releases from GitHub
func (c *Checker) fetchReleases(ctx context.Context) ([]*ReleaseInfo, error) {
	// Check if we're rate limited
	if c.isRateLimited() {
		return nil, &RateLimitError{
			Reset:     c.rateLimit.Reset,
			Remaining: c.rateLimit.Remaining,
		}
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases", c.apiBaseURL, c.owner, c.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", fmt.Sprintf("router/%s", version.Current.String()))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Update rate limit info from response headers
	c.updateRateLimitFromResponse(resp)

	// Handle rate limit errors
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if c.rateLimit != nil {
			return nil, &RateLimitError{
				Reset:     c.rateLimit.Reset,
				Remaining: c.rateLimit.Remaining,
			}
		}
		return nil, fmt.Errorf("GitHub API rate limit exceeded")
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("repository %s/%s not found or not public (HTTP 404)", c.owner, c.repo)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	// Limit response body size to prevent memory exhaustion from malicious/large responses
	limitedReader := io.LimitReader(resp.Body, maxAPIResponseSize)
	var ghReleases []githubRelease
	if err := json.NewDecoder(limitedReader).Decode(&ghReleases); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	releases := make([]*ReleaseInfo, 0, len(ghReleases))
	for _, gh := range ghReleases {
		release, err := c.parseGitHubRelease(&gh)
		if err != nil {
			// Skip releases that can't be parsed
			continue
		}
		releases = append(releases, release)
	}

	return releases, nil
}

// updateRateLimitFromResponse extracts rate limit info from GitHub API response headers
func (c *Checker) updateRateLimitFromResponse(resp *http.Response) {
	limit := resp.Header.Get("X-RateLimit-Limit")
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	reset := resp.Header.Get("X-RateLimit-Reset")

	if limit == "" && remaining == "" && reset == "" {
		return // Not a GitHub API response with rate limit headers
	}

	if c.rateLimit == nil {
		c.rateLimit = &RateLimitInfo{}
	}

	_, _ = fmt.Sscanf(limit, "%d", &c.rateLimit.Limit)
	_, _ = fmt.Sscanf(remaining, "%d", &c.rateLimit.Remaining)

	var resetUnix int64
	_, _ = fmt.Sscanf(reset, "%d", &resetUnix)
	if resetUnix > 0 {
		c.rateLimit.Reset = time.Unix(resetUnix, 0)
	}
}

// isRateLimited checks if we should wait before making another request
func (c *Checker) isRateLimited() bool {
	if c.rateLimit == nil {
		return false
	}
	if c.rateLimit.Remaining <= 0 && utils.Now().Before(c.rateLimit.Reset) {
		return true
	}
	return false
}

// GetRateLimit returns the current rate limit info (nil if unknown)
func (c *Checker) GetRateLimit() *RateLimitInfo {
	return c.rateLimit
}

// RateLimitError represents a GitHub API rate limit error
type RateLimitError struct {
	Reset     time.Time
	Remaining int
}

func (e *RateLimitError) Error() string {
	waitTime := max(time.Until(e.Reset), 0)
	return fmt.Sprintf("GitHub API rate limit exceeded, resets in %v", waitTime.Round(time.Second))
}

// githubRelease represents the GitHub API response for a release
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Prerelease  bool          `json:"prerelease"`
	Draft       bool          `json:"draft"`
	Assets      []githubAsset `json:"assets"`
}

// githubAsset represents a single asset in the GitHub API response
type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
}

// parseGitHubRelease converts a GitHub API release to our ReleaseInfo
func (c *Checker) parseGitHubRelease(gh *githubRelease) (*ReleaseInfo, error) {
	// Parse version from tag name (strip leading 'v')
	tagVersion := strings.TrimPrefix(gh.TagName, "v")
	ver, err := version.ParseVersion(tagVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to parse version from tag %s: %w", gh.TagName, err)
	}

	publishedAt, _ := time.Parse(time.RFC3339, gh.PublishedAt)

	release := &ReleaseInfo{
		Version:     ver,
		TagName:     gh.TagName,
		Name:        gh.Name,
		Body:        gh.Body,
		PublishedAt: publishedAt,
		HTMLURL:     gh.HTMLURL,
		Prerelease:  gh.Prerelease,
		Draft:       gh.Draft,
		Assets:      make([]ReleaseAsset, len(gh.Assets)),
	}

	for i, asset := range gh.Assets {
		release.Assets[i] = ReleaseAsset{
			Name:        asset.Name,
			Size:        asset.Size,
			DownloadURL: asset.BrowserDownloadURL,
			ContentType: asset.ContentType,
		}
	}

	return release, nil
}

// findLatestApplicableRelease finds the latest release that matches channel and platform
func (c *Checker) findLatestApplicableRelease(releases []*ReleaseInfo) *ReleaseInfo {
	var latest *ReleaseInfo

	for _, release := range releases {
		// Skip drafts
		if release.Draft {
			continue
		}

		// Check channel
		if c.channel == "stable" && release.Prerelease {
			continue
		}

		// Check if release has assets for current platform
		if !c.hasAssetForPlatform(release) {
			continue
		}

		// Track latest
		if latest == nil || release.Version.IsGreaterThan(latest.Version) {
			latest = release
		}
	}

	return latest
}

// hasAssetForPlatform checks if a release has an asset for the current platform
func (c *Checker) hasAssetForPlatform(release *ReleaseInfo) bool {
	if c.assetPattern == nil {
		return false
	}
	for _, asset := range release.Assets {
		if c.assetPattern.MatchString(asset.Name) {
			return true
		}
	}
	return false
}

// compileAssetPattern compiles and caches the regex pattern for matching platform-specific assets
func (c *Checker) compileAssetPattern() {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	// Map Go arch names to common release naming
	archName := arch
	if arch == "amd64" {
		archName = "(amd64|x86_64)"
	}

	pattern := fmt.Sprintf(`zzrouter.*%s.*%s.*\.(tar\.gz|zip)$`, osName, archName)
	c.assetPattern = regexp.MustCompile(pattern)
}

// GetAssetForPlatform returns the appropriate asset for the current platform
func (c *Checker) GetAssetForPlatform(release *ReleaseInfo) *ReleaseAsset {
	if c.assetPattern == nil {
		return nil
	}
	for _, asset := range release.Assets {
		if c.assetPattern.MatchString(asset.Name) {
			return &asset
		}
	}
	return nil
}

// GetChecksumAsset returns the checksums file asset
func (c *Checker) GetChecksumAsset(release *ReleaseInfo) *ReleaseAsset {
	for _, asset := range release.Assets {
		if asset.Name == "checksums.txt" {
			return &asset
		}
	}
	return nil
}

// GetBundleAsset returns the required offline verification bundle.
func (c *Checker) GetBundleAsset(release *ReleaseInfo) *ReleaseAsset {
	for _, asset := range release.Assets {
		if asset.Name == "checksums.txt.sigstore.json" {
			return &asset
		}
	}
	return nil
}

// GetSignatureAsset returns the signature file asset
func (c *Checker) GetSignatureAsset(release *ReleaseInfo) *ReleaseAsset {
	for _, asset := range release.Assets {
		if asset.Name == "checksums.txt.sig" {
			return &asset
		}
	}
	return nil
}

// GetCertificateAsset returns the certificate file asset
func (c *Checker) GetCertificateAsset(release *ReleaseInfo) *ReleaseAsset {
	for _, asset := range release.Assets {
		if asset.Name == "checksums.txt.pem" {
			return &asset
		}
	}
	return nil
}

// matchesVersionPin checks if a version matches a pin pattern
// Supported patterns:
//   - "1.2.3" - exact match
//   - "1.2.x" - matches any patch version
//   - "1.x" or "1.x.x" - matches any minor/patch version
func (c *Checker) matchesVersionPin(v *version.Version, pin string) bool {
	pin = strings.ToLower(strings.TrimSpace(pin))

	// Check for exact match
	if !strings.Contains(pin, "x") {
		pinVer, err := version.ParseVersion(pin)
		if err != nil {
			return false
		}
		return v.IsEqual(pinVer)
	}

	// Parse pin pattern
	parts := strings.Split(pin, ".")

	// Check major version
	if len(parts) >= 1 && parts[0] != "x" {
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			return false
		}
		if v.Major != major {
			return false
		}
	}

	// Check minor version
	if len(parts) >= 2 && parts[1] != "x" {
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
		if v.Minor != minor {
			return false
		}
	}

	// Check patch version
	if len(parts) >= 3 && parts[2] != "x" {
		patch, err := strconv.Atoi(parts[2])
		if err != nil {
			return false
		}
		if v.Patch != patch {
			return false
		}
	}

	return true
}
