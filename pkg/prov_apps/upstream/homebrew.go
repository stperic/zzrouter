package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stperic/zzrouter/pkg/config"
)

// maxFormulaBytes caps the response body. A formula document is a few KB;
// ollama's is under 10 KB. This bounds memory without being anywhere near a
// real payload.
const maxFormulaBytes = 4 << 20

// formulaDoc is the subset of the Homebrew formula API this package reads.
type formulaDoc struct {
	Versions struct {
		Stable string `json:"stable"`
	} `json:"versions"`
}

// latestHomebrew resolves the stable version a Homebrew formula currently
// ships.
//
// This is a *packager's* ceiling, not the project's. It is the right answer
// for a node that installs through Homebrew, precisely because the formula
// routinely trails the upstream release by a day or two: reporting the
// upstream tag to such a node offers an upgrade it cannot perform, and the
// upgrade then either does nothing or fails.
//
// The formula API is plain JSON over HTTPS with no authentication and no
// documented rate limit, so it needs none of the redirect handling the
// GitHub path does.
func (r *Resolver) latestHomebrew(ctx context.Context, src *config.VersionSource) (Release, error) {
	formulaURL := fmt.Sprintf("%s/api/formula/%s.json", r.homebrewBase, src.Formula)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, formulaURL, nil)
	if err != nil {
		return Release{}, fmt.Errorf("homebrew: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("homebrew: fetch %s: %w", src.Formula, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Release{}, fmt.Errorf("homebrew: no formula %q: %w", src.Formula, ErrNoUpstreamRelease)
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("homebrew: %s returned HTTP %d", src.Formula, resp.StatusCode)
	}

	var doc formulaDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFormulaBytes)).Decode(&doc); err != nil {
		return Release{}, fmt.Errorf("homebrew: unreadable formula document for %q: %w", src.Formula, err)
	}

	version := src.Normalize(doc.Versions.Stable)
	if version == "" {
		return Release{}, fmt.Errorf("homebrew: formula %q names no stable version: %w",
			src.Formula, ErrNoUpstreamRelease)
	}

	// The formula's stable version is already the bare identifier, so tag and
	// version coincide; there is no separate tag to install by. The URL is
	// the human-facing formula page rather than a release page, because that
	// is what an operator asking "why is this the ceiling" wants to see.
	return Release{
		Version: version,
		Tag:     doc.Versions.Stable,
		URL:     fmt.Sprintf("%s/formula/%s", r.homebrewBase, src.Formula),
	}, nil
}
