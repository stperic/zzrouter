package install

import (
	"fmt"
	"io/fs"
	"slices"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"gopkg.in/yaml.v3"
)

const shippedOnDemandDir = "files/providers/on-demand"

// ReleasePolicy grants exactly what this binary's shipped recipes use for the
// providers that install on goos/goarch, so a root installer can bootstrap
// override authority without inventing grants.
func ReleasePolicy(goos, goarch string) (InstallPolicy, error) {
	policy := InstallPolicy{Runtimes: map[string]RuntimePolicy{}}
	entries, err := fs.ReadDir(templates.AppsFS, shippedOnDemandDir)
	if err != nil {
		return policy, err
	}
	for _, entry := range entries {
		data, err := templates.AppsFS.ReadFile(shippedOnDemandDir + "/" + entry.Name() + "/config.yaml")
		if err != nil {
			return policy, err
		}
		var shipped config.OnDemandProvider
		if err := yaml.Unmarshal(data, &shipped); err != nil {
			return policy, fmt.Errorf("shipped %s config: %w", entry.Name(), err)
		}
		if shipped.Install == nil || !installsOn(shipped.Platforms, goos, goarch) {
			continue
		}
		for runtime, recipe := range shipped.Install.Runtimes {
			grant, err := releaseGrant(recipe)
			if err != nil {
				return policy, fmt.Errorf("shipped %s/%s recipe: %w", entry.Name(), runtime, err)
			}
			policy.Runtimes[entry.Name()+"/"+runtime] = grant
		}
	}
	return policy, nil
}

// MarshalReleasePolicy renders ReleasePolicy for a human to install as root.
func MarshalReleasePolicy(goos, goarch string) ([]byte, error) {
	policy, err := ReleasePolicy(goos, goarch)
	if err != nil {
		return nil, err
	}
	// The node rejects a policy without grants, so there is nothing to install.
	if len(policy.Runtimes) == 0 {
		return nil, fmt.Errorf("no shipped install recipe targets %s/%s; no policy is needed", goos, goarch)
	}
	body, err := yaml.Marshal(policy)
	if err != nil {
		return nil, err
	}
	header := "# zzRouter install policy: human-owned override authority.\n" +
		"# Generated from the shipped recipes of this release; it approves no package,\n" +
		"# import, entrypoint or index those recipes do not already use.\n" +
		"# Keep it root-owned and not writable by the node's service account.\n"
	return append([]byte(header), body...), nil
}

func installsOn(platforms []config.InstallPlatform, goos, goarch string) bool {
	return len(platforms) == 0 || slices.ContainsFunc(platforms, func(p config.InstallPlatform) bool {
		return p.OS == goos && p.Arch == goarch
	})
}

func releaseGrant(recipe config.InstallRecipe) (RuntimePolicy, error) {
	if recipe.Package == nil || recipe.StartupImport == nil || recipe.Indexes == nil || recipe.Indexes.Primary == nil ||
		recipe.Verify == nil || recipe.Verify.Imports == nil || recipe.OnlyBinary == nil {
		return RuntimePolicy{}, fmt.Errorf("incomplete recipe")
	}
	packages := map[string]string{*recipe.Package: ""}
	for name := range recipe.Companions {
		packages[name] = ""
	}
	indexes := []string{*recipe.Indexes.Primary}
	if recipe.Indexes.Extra != nil {
		indexes = append(indexes, *recipe.Indexes.Extra...)
	}
	return RuntimePolicy{
		Packages:          packages,
		Imports:           slices.Clone(*recipe.Verify.Imports),
		Entrypoints:       []string{*recipe.StartupImport},
		Indexes:           indexes,
		AllowSourceBuilds: !*recipe.OnlyBinary,
	}, nil
}
