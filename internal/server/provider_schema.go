package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// loadProviderSchema returns one provider's Go-spine ∪ schema.yaml shape
// on this node. Without a store or a schema.yaml, the Go spine alone.
func loadProviderSchema(store *pkgConfig.AppsConfigStore, provider string) *schema.ProviderSchema {
	var declared *schema.YAMLSchema
	if store != nil {
		data, err := store.ReadProviderSchemaBytes(provider)
		if err != nil {
			slog.Warn("provider schema unreadable; using the Go spine", "provider", provider, "error", err)
		} else if data != nil {
			if declared, err = schema.LoadYAMLSchema(data); err != nil {
				slog.Warn("provider schema malformed; using the Go spine", "provider", provider, "error", err)
			}
		}
	}
	merged, errs := schema.Merge(provider, declared)
	for _, err := range errs {
		// Silent here means a conflicting entry just drops a provider's long-tail param.
		slog.Warn("provider schema entry dropped", "provider", provider, "error", err)
	}
	return merged
}

// assetLocator returns the absolute path of a named asset of provider on
// this node, or the pkg/config/assets error that says why there is none,
// including that the node has no asset directory for the provider.
type assetLocator func(name string) (string, error)

// providerAssets locates names in provider's assets on this node.
func providerAssets(store *pkgConfig.AppsConfigStore, provider string) assetLocator {
	if store == nil {
		return func(string) (string, error) {
			return "", fmt.Errorf("%w: no provider store on this node", assets.ErrNotFound)
		}
	}
	dir, err := store.Assets(provider)
	if err != nil {
		return func(string) (string, error) { return "", fmt.Errorf("%w: %w", assets.ErrNotFound, err) }
	}
	return dir.Path
}

// assetParamLocalizer is the node's prov_apps.ParamLocalizer: each
// asset-typed parameter's name becomes the path of that asset in this
// node's own copy of the provider's assets.
func assetParamLocalizer(store *pkgConfig.AppsConfigStore) prov_apps.ParamLocalizer {
	return func(provider, endpoint, _ string, params map[string]string) (prov_apps.Localized, error) {
		shapes := loadProviderSchema(store, provider).ForEndpoint(endpoint)
		return localizeAssetParams(provider, params, shapes, providerAssets(store, provider))
	}
}

// maxContentArgBytes caps an asset handed to the engine as a flag value
// (schema pass: content). Linux refuses any one argv string of 128 KiB
// or more (MAX_ARG_STRLEN) with an exec error that names no argument.
// macOS bounds only the whole argv and environment (ARG_MAX, 1 MiB);
// Windows caps the whole command line at 32767 characters, so content
// that fits here can still fail CreateProcess there.
const maxContentArgBytes = 128<<10 - 1

// localizeAssetParams returns params with every asset-typed value replaced
// by its path from locate, or by its content where the schema says the
// engine takes the text, and the digest of each such file. A name that
// does not locate refuses the launch: passed through, the engine would
// read it as a path relative to its working directory.
func localizeAssetParams(provider string, params map[string]string, shapes map[string]schema.ParamShape, locate assetLocator) (prov_apps.Localized, error) {
	out := prov_apps.Localized{Params: params}
	for key, value := range params {
		shape := shapes[key]
		if shape.Kind != schema.ParamAsset {
			continue
		}
		path, err := locate(value)
		if err != nil {
			return prov_apps.Localized{}, fmt.Errorf("parameter %s names asset %q of provider %s: %w", key, value, provider, err)
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is a located asset, a regular file inside the provider's asset directory
		if err != nil {
			return prov_apps.Localized{}, fmt.Errorf("parameter %s: read asset %q: %w", key, value, err)
		}
		arg := path
		if shape.Pass == schema.AssetPassContent {
			if arg, err = contentArg(data); err != nil {
				return prov_apps.Localized{}, fmt.Errorf("parameter %s: asset %q cannot be passed by content: %w", key, value, err)
			}
		}
		if out.Files == nil {
			out.Params = maps.Clone(params)
			out.Files = map[string]string{}
		}
		out.Params[key] = arg
		out.Files[key] = assets.Digest(data)
	}
	return out, nil
}

// contentArg is an asset's bytes as one argv string, refused where exec
// would refuse it with an error naming nothing: over the per-argument
// limit, or holding a NUL. Empty content would read as a bare boolean
// flag, so it is refused too.
func contentArg(data []byte) (string, error) {
	switch {
	case len(data) == 0:
		return "", errors.New("it is empty")
	case len(data) > maxContentArgBytes:
		return "", fmt.Errorf("it is %d bytes, over the %d an argument may hold", len(data), maxContentArgBytes)
	case bytes.IndexByte(data, 0) >= 0:
		return "", errors.New("it holds a NUL byte")
	}
	return string(data), nil
}

func providerRuntimeChecks(store *pkgConfig.AppsConfigStore) install.RuntimeChecksResolver {
	return func(provider, runtime string) (schema.RuntimeChecks, error) {
		if store == nil {
			return schema.RuntimeChecks{}, fmt.Errorf("runtime diagnostics unavailable: no provider store")
		}
		data, err := store.ReadProviderSchemaBytes(provider)
		if err != nil {
			return schema.RuntimeChecks{}, err
		}
		declared, err := schema.LoadYAMLSchema(data)
		if err != nil {
			return schema.RuntimeChecks{}, err
		}
		if err := declared.Diagnostics.Validate(); err != nil {
			return schema.RuntimeChecks{}, err
		}
		if declared.Diagnostics == nil {
			return schema.RuntimeChecks{}, fmt.Errorf("runtime diagnostics unsupported on this node; upgrade the node")
		}
		checks, ok := declared.Diagnostics.Runtimes[runtime]
		if !ok {
			return schema.RuntimeChecks{}, fmt.Errorf("runtime %q not declared by provider %q", runtime, provider)
		}
		return checks, nil
	}
}

func providerMemoryBudget(store *pkgConfig.AppsConfigStore) func(string) (*schema.MemoryBudget, error) {
	return func(provider string) (*schema.MemoryBudget, error) {
		if store == nil {
			return nil, nil
		}
		data, err := store.ReadProviderSchemaBytes(provider)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		declared, err := schema.LoadYAMLSchema(data)
		if err != nil {
			return nil, err
		}
		if err := declared.Diagnostics.Validate(); err != nil {
			return nil, err
		}
		if declared.Diagnostics == nil {
			return nil, nil
		}
		return declared.Diagnostics.Memory, nil
	}
}
