package upstream

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/version"
)

// Status is the ordering verdict between a known identifier (what a node has
// installed, or what a provider pins) and the newest upstream release.
type Status string

const (
	// StatusNewer means upstream has published something later.
	StatusNewer Status = "newer"
	// StatusSame means the two identifiers match.
	StatusSame Status = "same"
	// StatusOlder means the known identifier is ahead of upstream's latest,
	// which happens with a hand-installed prerelease or an upstream yank.
	StatusOlder Status = "older"
	// StatusUnknown means no ordering could be established: a missing
	// operand, an unparseable identifier, an opaque comparator, or a lookup
	// that never completed.
	StatusUnknown Status = "unknown"
)

// Compare orders current against latest under src's comparator.
//
// Anything it cannot order yields StatusUnknown. That distinction carries
// weight: if a failed lookup collapsed into StatusSame, an unreachable
// upstream or an offline cluster would render as "you are up to date", which
// is the one wrong answer this whole package exists to avoid.
func Compare(src *config.VersionSource, current, latest string) Status {
	current = src.Normalize(current)
	latest = src.Normalize(latest)
	if current == "" || latest == "" {
		return StatusUnknown
	}
	if current == latest {
		return StatusSame
	}

	switch src.Comparator() {
	case config.CompareSemver:
		return compareSemver(current, latest)
	case config.ComparePEP440:
		return comparePEP440Status(current, latest)
	case config.CompareBuildNumber:
		return compareBuildNumber(current, latest)
	default:
		// Opaque: the two differ, but claiming a direction for a scheme
		// nobody has reasoned about would be a guess.
		return StatusUnknown
	}
}

func compareSemver(current, latest string) Status {
	cur, err := version.ParseVersion(current)
	if err != nil {
		return StatusUnknown
	}
	lat, err := version.ParseVersion(latest)
	if err != nil {
		return StatusUnknown
	}
	switch {
	case lat.IsGreaterThan(cur):
		return StatusNewer
	case cur.IsGreaterThan(lat):
		return StatusOlder
	default:
		return StatusSame
	}
}

func comparePEP440Status(current, latest string) Status {
	cur, ok := parsePEP440(current)
	if !ok {
		return StatusUnknown
	}
	lat, ok := parsePEP440(latest)
	if !ok {
		return StatusUnknown
	}
	switch comparePEP440(cur, lat) {
	case -1:
		return StatusNewer
	case 1:
		return StatusOlder
	default:
		return StatusSame
	}
}

func compareBuildNumber(current, latest string) Status {
	cur, err := strconv.Atoi(current)
	if err != nil {
		return StatusUnknown
	}
	lat, err := strconv.Atoi(latest)
	if err != nil {
		return StatusUnknown
	}
	switch {
	case lat > cur:
		return StatusNewer
	case cur > lat:
		return StatusOlder
	default:
		return StatusSame
	}
}

// Newest returns the greatest identifier in versions under src's comparator,
// or "" when none can be ordered.
//
// PEP 700 does not require PyPI's version list to be sorted, so picking the
// last element is wrong: at the time of writing, lexically sorting vllm's 95
// published versions puts 0.9.2 above 0.27.1.
//
// Identifiers the comparator cannot order are skipped. For a pypi source that
// set is genuinely empty, because config.Validate requires compare: pep440
// and every PyPI identifier is a PEP 440 one — which is the point, since
// skipping the newest entry is indistinguishable from being up to date.
func Newest(src *config.VersionSource, versions []string) string {
	best := ""
	for _, raw := range versions {
		v := strings.TrimSpace(raw)
		if v == "" || !orderable(src, v) {
			continue
		}
		if best == "" || Compare(src, best, v) == StatusNewer {
			best = v
		}
	}
	return best
}

// Explain says why Compare could not order a pair, in words an agent can act
// on. It returns "" whenever the two operands did order.
//
// StatusUnknown is otherwise indistinguishable from "we did not look", which
// leaves a caller with no way to tell a transient upstream outage from a
// version string this node will never be able to compare.
func Explain(src *config.VersionSource, current, latest string) string {
	if Compare(src, current, latest) != StatusUnknown {
		return ""
	}
	switch {
	case strings.TrimSpace(current) == "":
		return "no version recorded on this node"
	case strings.TrimSpace(latest) == "":
		return "no upstream release resolved"
	case src.Comparator() == config.CompareOpaque:
		return "this version_source declares no comparator, so releases cannot be ordered"
	case !orderable(src, current):
		return fmt.Sprintf("installed version %q is not a %s identifier", current, src.Comparator())
	case !orderable(src, latest):
		return fmt.Sprintf("upstream version %q is not a %s identifier", latest, src.Comparator())
	default:
		return "versions could not be ordered"
	}
}

// orderable reports whether v can participate in an ordering under src.
func orderable(src *config.VersionSource, v string) bool {
	switch src.Comparator() {
	case config.CompareSemver:
		parsed, err := version.ParseVersion(src.Normalize(v))
		return err == nil && parsed.PreRelease == ""
	case config.ComparePEP440:
		parsed, ok := parsePEP440(src.Normalize(v))
		return ok && !isPEP440PreRelease(parsed)
	case config.CompareBuildNumber:
		_, err := strconv.Atoi(src.Normalize(v))
		return err == nil
	default:
		return false
	}
}

// DisplayVersion renders a release in the same shape as the installed version
// it will be shown beside.
//
// The shape is not uniform across providers because the installers disagree
// on what they record: llama.cpp keeps the raw tag ("b10453") while ollama
// keeps the normalized version ("0.21.2"). Printing one form for both yields
// "0.21.2 -> v0.32.14", where the prefix reads as part of the change rather
// than as an artifact of which string was chosen.
//
// Matching the installed shape also matches the form the installer wants,
// because the version a provider records is the form its own URLs are keyed
// on — so this is the right value to prefill an upgrade with.
func DisplayVersion(installed string, r Release) string {
	if r.Tag == "" {
		return r.Version
	}
	if r.Version == "" || r.Tag == r.Version {
		return r.Tag
	}
	prefix := strings.TrimSuffix(r.Tag, r.Version)
	if prefix != "" && strings.HasPrefix(installed, prefix) {
		return r.Tag
	}
	return r.Version
}
