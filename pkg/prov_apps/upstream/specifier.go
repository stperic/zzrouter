package upstream

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

var specifierOperation = regexp.MustCompile(`^(===|~=|==|!=|<=|>=|<|>)\s*(.*)$`)

// VersionAllowed applies Python packaging comparisons, including public/local equality.
func VersionAllowed(version, constraint string) error {
	if err := config.ValidateSpecifier(constraint); err != nil {
		return err
	}
	actual, ok := parsePEP440(version)
	if !ok {
		return fmt.Errorf("invalid Python version %q", version)
	}
	for _, part := range strings.Split(constraint, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		match := specifierOperation.FindStringSubmatch(part)
		operation, wanted := match[1], match[2]
		allowed := false
		if operation == "===" {
			allowed = strings.EqualFold(version, wanted)
		} else {
			wildcard := strings.HasSuffix(wanted, ".*")
			expected, valid := parsePEP440(strings.TrimSuffix(wanted, ".*"))
			if !valid {
				return fmt.Errorf("invalid Python constraint %q", constraint)
			}
			comparison := comparePEP440(actual, expected)
			switch operation {
			case "==", "!=":
				allowed = comparison == 0
				if wildcard {
					allowed = releasePrefix(actual, expected, len(expected.release))
				} else if strings.Contains(wanted, "+") {
					allowed = allowed && normalizedLocal(version) == normalizedLocal(wanted)
				}
				if operation == "!=" {
					allowed = !allowed
				}
			case "<=":
				allowed = comparison <= 0
			case ">=":
				allowed = comparison >= 0
			case "<":
				allowed = comparison < 0
				if !isPEP440PreRelease(expected) && sameRelease(actual, expected) && isPEP440PreRelease(actual) {
					allowed = false
				}
			case ">":
				allowed = comparison > 0
				if expected.postNum == sortsLow && sameRelease(actual, expected) && actual.postNum != sortsLow {
					allowed = false
				}
			case "~=":
				allowed = comparison >= 0 && releasePrefix(actual, expected, len(expected.release)-1)
			}
		}
		if !allowed {
			return fmt.Errorf("version %q does not satisfy %q", version, constraint)
		}
	}
	return nil
}

func releasePrefix(actual, expected pep440Version, length int) bool {
	if actual.epoch != expected.epoch {
		return false
	}
	for i := 0; i < length; i++ {
		if segment(actual.release, i) != segment(expected.release, i) {
			return false
		}
	}
	return true
}
func sameRelease(actual, expected pep440Version) bool {
	return releasePrefix(actual, expected, max(len(actual.release), len(expected.release)))
}
func normalizedLocal(version string) string {
	_, local, _ := strings.Cut(strings.ToLower(version), "+")
	segments := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	for i, segment := range segments {
		if n, err := strconv.Atoi(segment); err == nil {
			segments[i] = strconv.Itoa(n)
		}
	}
	return strings.Join(segments, ".")
}

// LatestMatching resolves from the selected package and the intersected authority constraints.
func LatestMatching(ctx context.Context, pkg, constraint string) (Release, error) {
	if !config.ValidDistribution(pkg) {
		return Release{}, fmt.Errorf("invalid package")
	}
	if err := config.ValidateSpecifier(constraint); err != nil {
		return Release{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	return defaultResolver.latestPyPIMatching(ctx, &config.VersionSource{Type: config.VersionSourcePyPI, Package: pkg, Compare: config.ComparePEP440}, constraint)
}
