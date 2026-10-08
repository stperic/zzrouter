// Package templates provides embedded configuration templates for zzRouter.
//
// AppsFS exposes the per-provider YAML tree under
// files/providers/{on-demand,external,cloud,registries}/*.yaml plus a
// settings.yaml. InstallDefaults copies the tree to a destination
// directory using copy-if-missing semantics — safe on upgrade.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

//go:embed files/node-default.yaml
var NodeConfigTemplate string

//go:embed files/client-default.yaml
var ClientConfigTemplate string

//go:embed files/env-default
var EnvTemplate string

// AppsFS carries the per-provider YAML tree consumed by the directory
// loader. Pass-through to consumers that want to walk the tree or read
// individual kinds. Paths inside the FS are rooted at `files/providers/`.
//
//go:embed all:files/providers
var AppsFS embed.FS

// GetNodeTemplate returns the host configuration template
func GetNodeTemplate() string {
	return NodeConfigTemplate
}

// GetClientTemplate returns the client configuration template
func GetClientTemplate() string {
	return ClientConfigTemplate
}

// InstallDefaults walks the embedded providers/ tree and copies each
// file to dstDir/<relative-path>. Files that already exist on disk are
// preserved (copy-if-missing semantics) so user edits survive an
// upgrade-time reinstall. Subdirectories are created with 0755; files
// are written 0600 via atomic temp+rename.
func InstallDefaults(dstDir string) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", dstDir, err)
	}
	return fs.WalkDir(AppsFS, providersRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// embed.FS uses forward slashes — translate to host paths.
		rel := strings.TrimPrefix(p, providersRoot)
		rel = strings.TrimPrefix(rel, "/")
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))

		if d.IsDir() {
			if rel == "" {
				return nil
			}
			return os.MkdirAll(dst, 0755)
		}

		// Copy-if-missing: leave user edits alone.
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		data, err := fs.ReadFile(AppsFS, p)
		if err != nil {
			return fmt.Errorf("read embed %s: %w", p, err)
		}
		return utils.AtomicWriteFile(dst, data, providerFileMode)
	})
}

// providersRoot is where AppsFS keeps the provider tree.
const providersRoot = "files/providers"

// providerFileMode matches every other file the node writes into the
// providers tree; the engine runs as the same user.
const providerFileMode = 0o600
