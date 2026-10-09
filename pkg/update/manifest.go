package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// validate refuses a manifest naming another version or an impossible protocol window.
func (m ReleaseManifest) validate(target string) error {
	if m.Version != target {
		return fmt.Errorf("release manifest version %q does not match target %q", m.Version, target)
	}
	if m.MinClusterProtocol < 1 || m.ClusterProtocol < m.MinClusterProtocol {
		return fmt.Errorf("release %s has invalid protocol window [%d, %d]", target, m.MinClusterProtocol, m.ClusterProtocol)
	}
	return nil
}

// ReleaseManifest fetches a target release's release.json, verified through the signed checksums.
func (s *Scheduler) ReleaseManifest(ctx context.Context, target string) (ReleaseManifest, error) {
	release, err := s.exactRelease(ctx, target)
	if err != nil {
		return ReleaseManifest{}, err
	}
	asset := release.asset("release.json")
	if asset == nil {
		return ReleaseManifest{}, httperr.Error{Status: http.StatusConflict, Code: string(httperr.CodeReleaseManifestMissing), Message: fmt.Sprintf("release %s has no release.json; a signed protocol manifest is required for rollout", target)}
	}
	dir, err := os.MkdirTemp("", "zzrouter-release-")
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	d := s.downloader.in(dir)
	path, content, err := d.downloadMetadataFile(ctx, asset, "release manifest")
	if err != nil {
		return ReleaseManifest{}, err
	}
	if err := s.verifier.verifyRelease(ctx, d, release, &DownloadResult{FilePath: path}); err != nil {
		return ReleaseManifest{}, err
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := manifest.validate(target); err != nil {
		return ReleaseManifest{}, err
	}
	return manifest, nil
}
