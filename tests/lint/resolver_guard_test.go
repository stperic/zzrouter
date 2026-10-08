package lint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolverSingleImplementation pins plan §8 step-3's
// "preMergeParameters deleted; mergeAndResolveParams deleted" invariant.
// The unified (*ServiceConfig).Resolve is the only legal merge path.
// Exemptions: this test file and the plan doc itself.
func TestResolverSingleImplementation(t *testing.T) {
	root := findProjectRoot(t)
	banned := []string{"preMergeParameters", "mergeAndResolveParams"}

	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// A file this guard cannot reach is not a file it has cleared.
			hits = append(hits, path+": not walkable: "+werr.Error())
			return nil //nolint:nilerr // recorded in hits; keep walking to report every one
		}
		if d.IsDir() {
			// Skip VCS + dependency caches + any dotted directory (worktrees,
			// editor caches, etc.) so we only lint the tracked source tree.
			name := d.Name()
			if name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("tests", "lint", "resolver_guard_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			hits = append(hits, rel+": unreadable: "+rerr.Error())
			return nil //nolint:nilerr // recorded in hits; keep walking to report every one
		}
		s := string(body)
		for _, tok := range banned {
			if strings.Contains(s, tok) {
				hits = append(hits, rel+": references "+tok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("banned resolver symbols present, or files this guard could not clear (plan §8 step 3):\n%s", strings.Join(hits, "\n"))
	}
}
