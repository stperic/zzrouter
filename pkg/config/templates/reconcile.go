package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/utils"
)

// managedSpineKeys are the runtime sections zzRouter owns outright: how it
// invokes an engine, how it decides the engine is up, and how many requests
// that engine can be handed at once. They describe the engine's own
// contract, not anything an operator tunes — tuning happens through the
// parameter tiers (defaults / models / nodes), which reconcile never
// touches.
//
// Concurrency belongs here because it is a property of the engine, not a
// preference: mlx_lm and a single-slot llama-server generate one sequence
// at a time no matter what an operator would prefer. Leaving it
// operator-owned meant a fix to a wrong value only ever reached fresh
// installs.
//
// Owning them means a fix to a launch argument or a readiness probe reaches
// existing installs on the next start. Without that, every engine-level fix
// would need a bespoke migration, and installs would drift apart silently.
var managedSpineKeys = []string{"execution", "failure_patterns", "health_check", "activity_probe", "max_concurrent_requests", "queue_timeout", "data_dir", "model_store"}

// managedTopLevelKeys are managed sections that sit outside any section.
//
// version_source is here for the same reason execution is: where an upstream
// publishes its releases is a fact about that project, not a preference an
// operator holds. Leaving it operator-owned would mean only fresh installs
// could ever answer "is there a newer build?", which is precisely the
// drift this key exists to detect.
//
// model_defaults is a retired key: no release ships it, so listing it here
// strips it from an older install before the strict load would refuse it.
// Engine platforms and prerequisites must follow the shipped install recipe.
var managedTopLevelKeys = []string{"version_source", "model_defaults", "install", "run_scope", "platforms", "requirements"}

// managedCapabilityKeys are the managed keys under capabilities.
//
// wire_endpoints says which HTTP surfaces the engine's own server speaks,
// which is a fact about the engine the same way its launch arguments are.
// While operator-owned, a surface the engine gained in a release
// (llama-server's Anthropic Messages API) stayed refused on every existing
// install until someone edited YAML. The cost, shared with execution: an
// operator cannot narrow the list, and an install pinned to an older engine
// is told of surfaces it may lack. The rest of capabilities (formats,
// priority, model assignments) is assignment policy and stays the operator's.
var managedCapabilityKeys = []string{"wire_endpoints"}

// managedSections lists the sections whose managed keys sit one level
// down, under the section's own name.
var managedSections = []managedKeySection{
	{"runtime", managedSpineKeys},
	{"capabilities", managedCapabilityKeys},
}

type managedKeySection struct {
	name string
	keys []string
}

// managedKeySections enumerates whole-key ownership; an empty name denotes the root.
func managedKeySections(template map[string]any) []managedKeySection {
	sections := []managedKeySection{{keys: managedTopLevelKeys}}
	// A shipped seed advances with the release; unseeded providers keep operator pins.
	if _, seeded := template["pinned_version"]; seeded {
		sections = append(sections, managedKeySection{keys: []string{"pinned_version"}})
	}
	for _, section := range managedSections {
		// A template without a map section manages nothing under it.
		if _, ok := template[section.name].(map[string]any); ok {
			sections = append(sections, section)
		}
	}
	return sections
}

// ReconcileManagedSpine brings every shipped provider file in dstDir back
// in line with the embedded release. A config.yaml gets its managed keys
// rewritten, including pinned_version when the template declares it. Other
// keys (enabled, parameter tiers, node lists) as the operator left them; a schema.yaml
// is owned outright and rewritten whole when it differs. Everything else,
// assets included, is the operator's and untouched.
//
// Returns the relative paths of the files it changed.
func ReconcileManagedSpine(dstDir string) ([]string, error) {
	return reconcile(AppsFS, dstDir)
}

func reconcile(src fs.FS, dstDir string) ([]string, error) {
	var changed []string
	files, history, err := prepareManagedFiles(src, dstDir)
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))

		live, readErr := os.ReadFile(dst)
		if errors.Is(readErr, os.ErrNotExist) {
			continue // absent — InstallDefaults owns the first write
		}
		if readErr != nil {
			failures = append(failures, fmt.Errorf("read %s: %w", rel, readErr))
			continue
		}
		tmpl := files[rel]

		next, updated := tmpl, !bytes.Equal(live, tmpl)
		if shippedFileOwnership(rel) == managedKeys {
			var mergeErr error
			next, updated, mergeErr = mergeManagedSpine(live, tmpl, history[rel])
			if mergeErr != nil {
				// Skip this file rather than aborting the walk. WalkDir visits
				// lexically, so returning an error here would let one malformed
				// cloud/ config stop reconcile before external/ and on-demand/
				// are reached, silently withholding managed keys from the
				// providers that need them most.
				slog.Warn("skipping unreconcilable provider config", "file", rel, "err", mergeErr)
				continue
			}
		}
		if !updated {
			continue
		}
		if writeErr := utils.AtomicWriteFile(dst, next, providerFileMode); writeErr != nil {
			failures = append(failures, fmt.Errorf("write %s: %w", rel, writeErr))
			continue
		}
		changed = append(changed, rel)
	}
	return changed, errors.Join(failures...)
}

// ownership is how much of a shipped provider file reconcile owns.
type ownership int

const (
	notManaged ownership = iota
	// managedKeys: config.yaml, where only the managed keys are the
	// release's and everything else is the operator's.
	managedKeys
	// managedWhole: schema.yaml describes the engine's flags, the same
	// argument this file makes for execution.
	managedWhole
)

// shippedFileOwnership classifies rel, a path relative to the providers
// root, by its exact place in the <kind>/<name>/... layout, so a provider
// that happens to be named "assets" is still just a provider.
func shippedFileOwnership(rel string) ownership {
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) == providerFileDepth && parts[2] == "config.yaml":
		return managedKeys
	case len(parts) == providerFileDepth && parts[2] == "schema.yaml":
		return managedWhole
	}
	return notManaged
}

// providerFileDepth is the element count of <kind>/<name>/<file>.
const providerFileDepth = 3

// reconcileFeatures refreshes shipped declarations while retaining deployment policy.
func reconcileFeatures(live, template map[string]any) bool {
	shipped, managed := template["features"].(map[string]any)
	if !managed {
		return false
	}
	current, exists := live["features"]
	if !exists {
		live["features"] = shipped
		return true
	}
	features, ok := current.(map[string]any)
	if !ok || len(features) == 0 {
		return false
	}
	changed := false
	for name := range features {
		if _, exists := shipped[name]; !exists {
			delete(features, name)
			changed = true
		}
	}
	for name, body := range shipped {
		declaration, ok := body.(map[string]any)
		if !ok {
			continue
		}
		next := make(map[string]any, len(declaration))
		for key, value := range declaration {
			next[key] = value
		}
		if previous, ok := features[name].(map[string]any); ok {
			if policy, exists := previous["default"]; exists {
				next["default"] = policy
			}
		}
		if !reflect.DeepEqual(features[name], next) {
			features[name] = next
			changed = true
		}
	}
	return changed
}

// mergeManagedSpine copies the managed keys from tmpl into live,
// reporting whether anything actually differed.
func mergeManagedSpine(live, tmpl []byte, history [][]string) (merged []byte, updated bool, err error) {
	var liveDoc, tmplDoc map[string]any
	if err := yaml.Unmarshal(live, &liveDoc); err != nil {
		return nil, false, fmt.Errorf("parse live config: %w", err)
	}
	// An empty, comment-only or explicitly-null file unmarshals to a nil map,
	// and assigning into a nil map panics. A zero-byte config.yaml is
	// reachable in the field: a crash mid-atomicWrite or a truncated
	// peer-sync leaves one behind, and the panic would take out node startup
	// rather than this one file.
	if liveDoc == nil {
		liveDoc = map[string]any{}
	}
	if err := yaml.Unmarshal(tmpl, &tmplDoc); err != nil {
		return nil, false, fmt.Errorf("parse template: %w", err)
	}

	paths := managedPaths(tmplDoc)
	for _, path := range history {
		if slices.ContainsFunc(paths, func(next []string) bool { return slices.Equal(next, path) }) {
			continue
		}
		parent := liveDoc
		if len(path) == 2 {
			parent, _ = liveDoc[path[0]].(map[string]any)
		}
		updated = mergeKeys(parent, nil, path[len(path)-1:]) || updated
	}
	updated = reconcileFeatures(liveDoc, tmplDoc) || updated
	for _, section := range managedKeySections(tmplDoc) {
		liveSection, tmplSection := liveDoc, tmplDoc
		if section.name != "" {
			tmplSection, _ = tmplDoc[section.name].(map[string]any)
			liveSection, _ = liveDoc[section.name].(map[string]any)
			if liveSection == nil {
				liveSection = map[string]any{}
			}
		}
		if mergeKeys(liveSection, tmplSection, section.keys) {
			if section.name != "" {
				liveDoc[section.name] = liveSection
			}
			updated = true
		}
	}
	if !updated {
		return live, false, nil
	}

	out, err := yaml.Marshal(liveDoc)
	if err != nil {
		return nil, false, fmt.Errorf("serialize merged config: %w", err)
	}
	return append(leadingComments(live), out...), true, nil
}

// mergeKeys makes each of keys in live what it is in tmpl, deleting the
// ones tmpl lacks, and reports whether live changed.
func mergeKeys(live, tmpl map[string]any, keys []string) bool {
	changed := false
	for _, key := range keys {
		want, present := tmpl[key]
		switch {
		case !present:
			if _, had := live[key]; had {
				delete(live, key)
				changed = true
			}
		case !reflect.DeepEqual(live[key], want):
			live[key] = want
			changed = true
		}
	}
	return changed
}

// leadingComments returns the file's header comment block. Provider configs
// are machine-written (the coordinator marshals a struct), so the header is
// the only commentary a round-trip can preserve — and it is the one that
// matters: it tells operators the file is managed.
func leadingComments(data []byte) []byte {
	var header []byte
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			break
		}
		header = append(header, line...)
	}
	return header
}
