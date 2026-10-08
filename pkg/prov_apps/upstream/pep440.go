package upstream

import (
	"regexp"
	"strconv"
	"strings"
)

// PEP 440 is Python's version scheme, and it is NOT semver: it allows a
// variable number of release segments, an epoch, and post/dev suffixes that
// strict major.minor.patch parsing cannot represent.
//
// This matters concretely rather than theoretically. Of vllm's 95 published
// versions, twelve are not strict three-segment: 0.6.1.post1, 0.6.1.post2,
// 0.10.1.1, 0.9.0.1 and friends. Ordering those with a semver parser drops
// them, and dropping the newest one makes an out-of-date node read as "up to
// date" — the single answer this package exists to prevent.
//
// Local version identifiers (the "+cu118" suffix) are parsed and then ignored
// for ordering, matching PEP 440's rule that they do not participate in
// comparisons against non-local versions.
var pep440Pattern = regexp.MustCompile(
	`^v?(?:(\d+)!)?(\d+(?:\.\d+)*)` + // epoch, release
		`(?:[-_.]?(a|b|c|rc|alpha|beta|pre|preview)[-_.]?(\d*))?` + // pre
		`(?:(?:-(\d+))|(?:[-_.]?(post|rev|r)[-_.]?(\d*)))?` + // post
		`(?:[-_.]?(dev)[-_.]?(\d*))?` + // dev
		`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`, // local
)

// preRank orders the prerelease spellings PEP 440 treats as equivalent.
var preRank = map[string]int{
	"alpha": 0, "a": 0,
	"beta": 1, "b": 1,
	"c": 2, "rc": 2, "pre": 2, "preview": 2,
}

// Sentinels for the three-way "absent sorts high / absent sorts low" rules in
// PEP 440's ordering. Using explicit bounds keeps the comparison a plain
// lexicographic walk instead of a nest of special cases.
const (
	sortsLow  = -1
	sortsHigh = 1 << 30
)

// pep440Version is a parsed version reduced to its ordering key.
type pep440Version struct {
	epoch   int
	release []int
	// preClass is sortsLow for a dev release with no pre segment, sortsHigh
	// for a final release, and 0 when a real prerelease is present.
	preClass int
	preRank  int
	preNum   int
	postNum  int // sortsLow when absent: a final release precedes its posts
	devNum   int // sortsHigh when absent: a dev release precedes its release
}

// parsePEP440 parses a public version identifier. Reports false for anything
// outside the scheme, which the caller treats as unorderable rather than
// guessing.
func parsePEP440(v string) (pep440Version, bool) {
	m := pep440Pattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(v)))
	if m == nil {
		return pep440Version{}, false
	}

	out := pep440Version{postNum: sortsLow, devNum: sortsHigh}

	if m[1] != "" {
		epoch, err := strconv.Atoi(m[1])
		if err != nil {
			return pep440Version{}, false
		}
		out.epoch = epoch
	}
	for _, seg := range strings.Split(m[2], ".") {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return pep440Version{}, false
		}
		out.release = append(out.release, n)
	}

	hasPre := m[3] != ""
	if hasPre {
		var ok bool
		out.preRank = preRank[m[3]]
		if out.preNum, ok = atoiOrZero(m[4]); !ok {
			return pep440Version{}, false
		}
	}

	hasPost := false
	var ok bool
	switch {
	case m[5] != "": // the "-N" spelling of a post release
		if out.postNum, ok = atoiOrZero(m[5]); !ok {
			return pep440Version{}, false
		}
		hasPost = true
	case m[6] != "":
		if out.postNum, ok = atoiOrZero(m[7]); !ok {
			return pep440Version{}, false
		}
		hasPost = true
	}

	hasDev := m[8] != ""
	if hasDev {
		if out.devNum, ok = atoiOrZero(m[9]); !ok {
			return pep440Version{}, false
		}
	}

	switch {
	case hasPre:
		out.preClass = 0
	case hasDev && !hasPost:
		// A dev release with no prerelease segment sorts below every
		// prerelease of the same version, not above them.
		out.preClass = sortsLow
	default:
		out.preClass = sortsHigh
	}

	return out, true
}

// atoiOrZero maps PEP 440's implicit-zero suffixes ("rc" == "rc0") to 0.
// Reports false for a number too large for int, which the caller treats as
// unorderable — silently folding an overflow to 0 would mis-rank it below
// every other release.
func atoiOrZero(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// comparePEP440 returns -1, 0 or 1 ordering a against b.
func comparePEP440(a, b pep440Version) int {
	if c := cmpInt(a.epoch, b.epoch); c != 0 {
		return c
	}
	// Release segments compare element-wise with missing tails treated as
	// zero, so 1.2 and 1.2.0 are equal and 0.10.1.1 outranks 0.10.1.
	for i := 0; i < max(len(a.release), len(b.release)); i++ {
		if c := cmpInt(segment(a.release, i), segment(b.release, i)); c != 0 {
			return c
		}
	}
	if c := cmpInt(a.preClass, b.preClass); c != 0 {
		return c
	}
	if a.preClass == 0 {
		if c := cmpInt(a.preRank, b.preRank); c != 0 {
			return c
		}
		if c := cmpInt(a.preNum, b.preNum); c != 0 {
			return c
		}
	}
	if c := cmpInt(a.postNum, b.postNum); c != 0 {
		return c
	}
	return cmpInt(a.devNum, b.devNum)
}

func segment(r []int, i int) int {
	if i < len(r) {
		return r[i]
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// isPEP440PreRelease reports whether v is a prerelease or dev release, which
// a stable-only feed must not recommend.
func isPEP440PreRelease(v pep440Version) bool {
	return v.preClass != sortsHigh || v.devNum != sortsHigh
}
