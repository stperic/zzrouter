package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/blang/semver"
	"github.com/stperic/zzrouter/pkg/version"
)

// ValidateVersion accepts only a canonical release version, never a location.
func ValidateVersion(target string) error {
	if len(target) > 128 {
		return fmt.Errorf("release version exceeds 128 characters")
	}
	v, err := semver.Parse(target)
	if err != nil || v.String() != target {
		return fmt.Errorf("invalid release version %q", target)
	}
	return nil
}

// Release resolves an exact tag independently of channel and list pagination.
func (c *Checker) Release(ctx context.Context, target string) (*ReleaseInfo, error) {
	if err := ValidateVersion(target); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", c.apiBaseURL, c.owner, c.repo, url.PathEscape("v"+target))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release v%s returned HTTP %d", target, resp.StatusCode)
	}
	var gh githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseSize)).Decode(&gh); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if gh.TagName != "v"+target || gh.Draft {
		return nil, fmt.Errorf("release does not match published tag v%s", target)
	}
	return c.parseGitHubRelease(&gh)
}

// ApplyVersion installs the exact release using the existing verifier and installer.
func (s *Scheduler) ApplyVersion(ctx context.Context, target string) error {
	if !s.beginApply() {
		return ErrApplyInFlight
	}
	defer s.endApply()
	release, err := s.exactRelease(ctx, target)
	if err != nil {
		return err
	}
	h := s.openUpdateJob(ctx, release)
	err = s.applyUpdate(ctx, release, h)
	if h != nil {
		if err != nil {
			h.Fail(err)
		} else {
			h.Done()
		}
	}
	return err
}

func (s *Scheduler) exactRelease(ctx context.Context, target string) (*ReleaseInfo, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	return s.checker.Release(ctx, target)
}

// ApplyVersionAsync dispatches an exact version, including on delegated installs.
func (s *Scheduler) ApplyVersionAsync(ctx context.Context, target string) (string, error) {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if err := ValidateVersion(target); err != nil {
		return "", err
	}
	if s.delegate != nil {
		return s.delegate.ApplyVersion(target)
	}
	if !s.beginApply() {
		status := s.GetStatus()
		if status.Operation != nil && status.Operation.ToVersion == target && !status.Operation.Finished() {
			return status.Operation.JobID, nil
		}
		return "", ErrApplyInFlight
	}
	release, err := s.exactRelease(ctx, target)
	if err != nil {
		s.endApply()
		return "", err
	}
	//nolint:contextcheck // the scheduler lifecycle owns the apply after HTTP acceptance
	return s.startResolvedApply(release)
}

// MatchesVersion compares the release identity without the binary's build timestamp.
func MatchesVersion(actual *version.Version, target string) bool {
	expected, err := version.ParseVersion(target)
	return err == nil && actual != nil && actual.IsEqual(expected)
}
