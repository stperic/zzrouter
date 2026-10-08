package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Architecture guard tests enforce the provider-lifecycle choke-point
// invariant documented on (*Server).FinalizeOnboarding:
//
//   Every provider "go live" transition on a node funnels through
//   FinalizeOnboarding. The symmetric "go offline" transition funnels
//   through FinalizeOffboarding. Both return error. Every caller
//   propagates to the HTTP response. Silent failures are impossible.
//
// Two layers of guard:
//
//   1. TestSetProviderEnabledChokepoint — forbids any caller outside the
//      two implementation files from touching the store's low-level
//      SetProviderEnabled. Tight filename allowlist; those files own
//      the mechanism.
//
//   2. TestServiceConfigSetEnabledAnnotated — catches the deeper bypass
//      via ServiceConfig.SetEnabled. Implementation files are allowed
//      silently; any other call site must carry an
//      `// architecture-exempt: <reason>` comment on the call line or
//      the line immediately above. This converts the old filename
//      allowlist (which required editing the test for every new
//      legitimate caller) into a self-justifying annotation at the
//      call site itself. Reviewers see the exemption in the diff.
//
// When a guard fails, read the FinalizeOnboarding doc comment. Either
// route your new code through FinalizeOnboarding / FinalizeOffboarding,
// or — if the code is genuinely init-time and cannot use the choke-point
// — add an `// architecture-exempt: <reason>` annotation and explain why
// the listener chain doesn't apply.

// repoRoot returns the absolute path to the zzrouter repo root by walking
// up from the current test package (internal/server → ../../).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "..", "..")
}

// scanGo walks the repo root for *.go files (excluding vendor, testdata,
// _test.go files, and hidden dirs) and yields (relative path, contents).
func scanGo(t *testing.T, root string, yield func(relPath string, content []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || name == ".git" ||
				name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		yield(filepath.ToSlash(rel), b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// isCommentLine reports whether a trimmed source line is a pure comment
// (leading // or part of a * continuation in a block comment). Used to
// skip matches inside docstrings that legitimately quote the forbidden
// symbol.
func isCommentLine(trim string) bool {
	return strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "*")
}

// TestSetProviderEnabledChokepoint enforces that AppsConfigStore.
// SetProviderEnabled is only called from the two choke-point functions
// (FinalizeOnboarding, FinalizeOffboarding) and their defining store.
func TestSetProviderEnabledChokepoint(t *testing.T) {
	t.Parallel()

	// Files allowed to call SetProviderEnabled. Any other caller is a
	// scattered enable path that bypasses the listener chain and must
	// route through the choke-point instead.
	allow := map[string]string{
		"internal/server/server_providers.go": "choke-point: FinalizeOnboarding, FinalizeOffboarding",
		"pkg/config/apps_config_store.go":     "defines SetProviderEnabled",
	}

	re := regexp.MustCompile(`\bSetProviderEnabled\b`)

	var violations []string
	scanGo(t, repoRoot(t), func(rel string, content []byte) {
		if _, ok := allow[rel]; ok {
			return
		}
		for i, line := range strings.Split(string(content), "\n") {
			if isCommentLine(strings.TrimSpace(line)) {
				continue
			}
			if re.MatchString(line) {
				violations = append(violations,
					rel+":"+strconv.Itoa(i+1)+" — "+strings.TrimSpace(line))
			}
		}
	})

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("SetProviderEnabled must only be called from the choke-point. Allowed:\n")
		for f, why := range allow {
			b.WriteString("  - " + f + " (" + why + ")\n")
		}
		b.WriteString("Violations:\n  ")
		b.WriteString(strings.Join(violations, "\n  "))
		t.Fatal(b.String())
	}
}

// TestServiceConfigSetEnabledAnnotated enforces that ServiceConfig.
// SetEnabled — the in-memory typed setter — is only called from:
//
//   - The two implementation files (store mutation path + definition).
//   - A call site that carries an `// architecture-exempt: <reason>`
//     annotation on the same line or the line immediately preceding.
//
// The annotation shifts the justification from a test-maintained
// filename list to the call site itself. A new legitimate init-time
// caller is a one-line `// architecture-exempt` comment away; a
// reviewer sees the exemption in the PR diff. A drive-by caller that
// forgets the annotation fails CI with a message pointing at the
// choke-point.
func TestServiceConfigSetEnabledAnnotated(t *testing.T) {
	t.Parallel()

	// Implementation files: they define / route the mutation and do
	// not need an annotation. Every OTHER caller does.
	implementation := map[string]bool{
		"pkg/config/apps_config_store.go": true,
		"pkg/config/service_config.go":    true,
	}

	callRe := regexp.MustCompile(`\.SetEnabled\(`)
	annotationRe := regexp.MustCompile(`//\s*architecture-exempt\s*:\s*\S`)

	var violations []string
	scanGo(t, repoRoot(t), func(rel string, content []byte) {
		if implementation[rel] {
			return
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if isCommentLine(strings.TrimSpace(line)) {
				continue
			}
			if !callRe.MatchString(line) {
				continue
			}
			// Accept annotation on the call line itself (trailing comment)
			// or on the line immediately above.
			annotated := annotationRe.MatchString(line)
			if !annotated && i > 0 {
				annotated = annotationRe.MatchString(lines[i-1])
			}
			// Also tolerate annotation up to 3 lines above, since the
			// natural place to put it is right above an `if` guard that
			// wraps the SetEnabled call.
			if !annotated {
				for j := 2; j <= 3 && i-j >= 0; j++ {
					if annotationRe.MatchString(lines[i-j]) {
						annotated = true
						break
					}
				}
			}
			if !annotated {
				violations = append(violations,
					rel+":"+strconv.Itoa(i+1)+" — "+strings.TrimSpace(line))
			}
		}
	})

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("ServiceConfig.SetEnabled must either:\n")
		b.WriteString("  (a) be inside an implementation file (store or definition), or\n")
		b.WriteString("  (b) carry an `// architecture-exempt: <reason>` annotation\n")
		b.WriteString("      on the call line or up to 3 lines above it.\n\n")
		b.WriteString("Unannotated call sites (route through FinalizeOnboarding/FinalizeOffboarding instead):\n  ")
		b.WriteString(strings.Join(violations, "\n  "))
		t.Fatal(b.String())
	}
}
