package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/assets"
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
var managedSpineKeys = []string{"execution", "health_check", "max_concurrent_requests", "queue_timeout"}

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
var managedTopLevelKeys = []string{"version_source", "model_defaults", "install"}

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
var managedSections = []struct {
	name string
	keys []string
}{
	{"runtime", managedSpineKeys},
	{"capabilities", managedCapabilityKeys},
}

// ReconcileManagedSpine brings every shipped provider file in dstDir back
// in line with the embedded release. A config.yaml gets its managed keys
// rewritten, including pinned_version when the template declares it. Other
// keys (enabled, parameter tiers, node lists) as the operator left them; a schema.yaml and a
// shipped asset are owned outright and rewritten whole when they differ.
// Files with no embedded counterpart, such as operator-added providers
// and assets, are untouched.
//
// Returns the relative paths of the files it changed.
func ReconcileManagedSpine(dstDir string) ([]string, error) {
	return reconcile(AppsFS, dstDir)
}

func reconcile(src fs.FS, dstDir string) ([]string, error) {
	var changed []string

	err := fs.WalkDir(src, providersRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, providersRoot), "/")
		ownership := shippedFileOwnership(rel)
		if ownership == notManaged {
			return nil
		}
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))

		live, readErr := os.ReadFile(dst)
		if errors.Is(readErr, os.ErrNotExist) {
			return nil // absent — InstallDefaults owns the first write
		}
		if readErr != nil {
			return fmt.Errorf("read %s: %w", rel, readErr)
		}
		tmpl, readErr := fs.ReadFile(src, p)
		if readErr != nil {
			return fmt.Errorf("read embed %s: %w", p, readErr)
		}

		next, updated := tmpl, !bytes.Equal(live, tmpl)
		if ownership == managedKeys {
			var mergeErr error
			next, updated, mergeErr = mergeManagedSpine(live, tmpl)
			if mergeErr != nil {
				// Skip this file rather than aborting the walk. WalkDir visits
				// lexically, so returning an error here would let one malformed
				// cloud/ config stop reconcile before external/ and on-demand/
				// are reached, silently withholding managed keys from the
				// providers that need them most.
				slog.Warn("skipping unreconcilable provider config", "file", rel, "err", mergeErr)
				return nil
			}
		}
		if !updated {
			return nil
		}
		if writeErr := utils.AtomicWriteFile(dst, next, providerFileMode); writeErr != nil {
			slog.Warn("skipping provider file that could not be written", "file", rel, "err", writeErr)
			return nil
		}
		changed = append(changed, rel)
		return nil
	})

	return changed, err
}

// ownership is how much of a shipped provider file reconcile owns.
type ownership int

const (
	notManaged ownership = iota
	// managedKeys: config.yaml, where only the managed keys are the
	// release's and everything else is the operator's.
	managedKeys
	// managedWhole: schema.yaml describes the engine's flags, the same
	// argument this file makes for execution, and a shipped asset is
	// content the release vouches for; operators add their own under
	// other names.
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
	case len(parts) == providerFileDepth+1 && parts[2] == assets.DirName:
		return managedWhole
	}
	return notManaged
}

// providerFileDepth is the element count of <kind>/<name>/<file>.
const providerFileDepth = 3

// IsShippedAsset reports whether the release ships an asset of that name
// for the provider. Those names are the release's to change.
func IsShippedAsset(provider, name string) bool {
	return isShippedAsset(AppsFS, provider, name)
}

func isShippedAsset(src fs.FS, provider, name string) bool {
	if assets.ValidateName(name) != nil || provider != path.Base(provider) || provider == "." || provider == ".." {
		return false
	}
	kinds, err := fs.ReadDir(src, providersRoot)
	if err != nil {
		return false
	}
	for _, kind := range kinds {
		if !kind.IsDir() {
			continue
		}
		fi, err := fs.Stat(src, path.Join(providersRoot, kind.Name(), provider, assets.DirName, name))
		if err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

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
func mergeManagedSpine(live, tmpl []byte) (merged []byte, updated bool, err error) {
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

	updated = mergeKeys(liveDoc, tmplDoc, managedTopLevelKeys)
	// A shipped seed must advance with the release; unseeded providers keep operator pins.
	if _, seeded := tmplDoc["pinned_version"]; seeded && mergeKeys(liveDoc, tmplDoc, []string{"pinned_version"}) {
		updated = true
	}
	if reconcileFeatures(liveDoc, tmplDoc) {
		updated = true
	}
	for _, section := range managedSections {
		// A template without the section manages nothing under it.
		tmplSection, ok := tmplDoc[section.name].(map[string]any)
		if !ok {
			continue
		}
		liveSection, ok := liveDoc[section.name].(map[string]any)
		if !ok {
			liveSection = map[string]any{}
		}
		if mergeKeys(liveSection, tmplSection, section.keys) {
			liveDoc[section.name] = liveSection
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
