package recovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/stperic/common/secrets"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

// A handoff's source and separate list are shown to the operator, so an
// archive keeps them short plain text.
const maxPlainText, maxSeparate = 1024, 32

// handoffEntry reports whether a handoff bundle carries the entry name: the
// database image, model groups, spend, pricing overrides, the encrypted
// provider connections, and the provider tree's settings, ownership receipt,
// definitions and assets. It admits only what it names, so a file added to
// the configuration later does not travel.
func handoffEntry(name string) bool {
	switch name {
	case imageName, "config/model_groups.yaml", "config/spend.json", "config/pricing_overrides.yaml", "config/credentials.enc",
		"config/providers/settings.yaml", "config/providers/" + templates.OwnershipFile:
		return true
	}
	parts, ok := providerParts(name)
	switch {
	case !ok:
		return false
	case len(parts) == 3:
		return parts[2] == "config.yaml" || parts[2] == "schema.yaml"
	case len(parts) == 4:
		return parts[2] == assets.DirName && assets.ValidateName(parts[3]) == nil
	}
	return false
}

// handoffDir reports whether a handoff carries entries under the directory
// name: the provider tree, a kind, a provider or its assets.
func handoffDir(name string) bool {
	parts, ok := providerParts(name)
	return name == "config/providers" || ok && (len(parts) <= 2 || len(parts) == 3 && parts[2] == assets.DirName)
}

// providerParts splits a name under config/providers/<kind>/ into its parts
// from the kind on: <kind>/<name>/<file> or <kind>/<name>/assets/<asset>.
func providerParts(name string) ([]string, bool) {
	rest, ok := strings.CutPrefix(name, "config/providers/")
	parts := strings.Split(rest, "/")
	return parts, ok && config.IsProviderKindDir(parts[0])
}

// validateHandoff applies a handoff bundle's own rules: the image and its
// store, no inventory, its source, separate list and entry names as plain
// text, only what handoffEntry admits, and at least one provider definition,
// so an archive that lost its provider tree is refused.
func validateHandoff(manifest Manifest, image bool) error {
	if !image || manifest.Inventory != nil || len(manifest.MissingOptional) > 0 {
		return errors.New("a handoff bundle holds the access database image and its store, and no inventory")
	}
	source := manifest.Source
	if source == nil || !plainText(source.Release) || !plainText(source.Node) || !plainText(source.Platform) ||
		len(manifest.Separate) == 0 || len(manifest.Separate) > maxSeparate || slices.ContainsFunc(manifest.Separate, func(s string) bool { return !plainText(s) }) {
		return errors.New("a handoff bundle names its source and what moves separately in plain text")
	}
	for _, file := range manifest.Files {
		if !handoffEntry(file.Name) || !plainText(file.Name) {
			return fmt.Errorf("a handoff bundle does not carry %q", file.Name)
		}
	}
	if !slices.ContainsFunc(manifest.Files, func(file File) bool {
		parts, ok := providerParts(file.Name)
		return ok && len(parts) == 3 && parts[2] == "config.yaml"
	}) {
		return errors.New("a handoff bundle without a provider definition has lost its provider tree")
	}
	return nil
}

// plainText reports whether s is short, non-empty text with no control or
// other non-printing character.
func plainText(s string) bool {
	return s != "" && len(s) <= maxPlainText && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
}

// handoffSeparate is what the operator of a handoff moves or recreates by
// hand, since no handoff carries it.
func handoffSeparate() []string {
	return []string{
		"The destination's own node.yaml: node name, listen address, TLS, storage paths, authentication and cluster role.",
		"Environment variables the dotenv files or the service environment set: the static installation keys (ZZROUTER_ADMIN_API_KEY, ZZROUTER_API_KEY), provider credentials (such as OPENAI_API_KEY or HF_TOKEN) and every variable a provider definition reads. Set them on the destination: a connection that reads its key from the environment fails without it.",
		"External services the provider definitions point at, such as an Ollama server or an endpoint on the old machine's network: make sure the destination reaches them.",
		"External TLS or proxy trust material that node.yaml names: copy it over a secure channel and name it in the destination's node.yaml.",
		"Provider runtimes and model weights: install and download them on the destination.",
		"Workers: pair each one again with the new coordinator. The cluster CA, node identity and worker enrollments stay with the old one.",
	}
}

// target is a file or directory of the installation a handoff activation
// publishes, by its slash path relative to the installation directory; a
// directory's name ends in "/". Before is the SHA-256 of the file it held
// before the activation, "" for none. A file's entry is the staging entry it
// becomes, whose SHA-256 is sum; without one, the activation removes it.
type target struct {
	Name   string `json:"name"`
	Before string `json:"before,omitempty"`
	entry  string
	sum    string
}

// replaces reports whether t's previous content goes to the rollback directory.
func (t target) replaces() bool { return t.Before != "" && t.Before != t.sum }

func (t target) isDir() bool { return strings.HasSuffix(t.Name, "/") }

// keptLost is a target an activation replaced whose previous content is
// neither in place nor kept in its rollback directory.
type keptLost string

func (k keptLost) Error() string { return string(k) + ": what it held is not kept" }

// Reference is a node name in a handoff's routes or provider definitions that
// named a machine of the source's cluster. Activation reports it and never
// rewrites it: Where says which and what it means here, Node holds the name
// ("" for a route replica pinned nowhere), and Fix is the zz_* call that
// changes it once the node runs.
type Reference struct {
	Where string `json:"where"`
	Node  string `json:"node"`
	Fix   string `json:"fix"`
}

// checkHandoff runs a handoff's own checks of the installation in dir,
// read-only, and returns its targets and the node references of the staged
// configuration. The installation must start as a coordinator and not be
// paired, an explicit credential key must be the handoff's, and, unless
// replace, the handoff may replace no access store of another ID (foreign),
// provider connections, or cloud or registry definition of the installation's
// own. Resuming pending, it takes the targets pending recorded.
func checkHandoff(ctx context.Context, dir, staging string, m Manifest, pending *activation, replace, foreign bool) ([]target, []Reference, error) {
	cfg, err := config.ReadExistingNodeConfig(dir)
	if err != nil {
		return nil, nil, err
	}
	sources, err := handoffSources(dir, cfg.Security.Credentials, m)
	if err != nil {
		return nil, nil, err
	}
	refs, err := references(staging, m.Source.Node, cfg.Node.Name)
	if err != nil {
		return nil, nil, err
	}
	if pending != nil {
		targets := slices.Clone(pending.Targets)
		for i, t := range targets {
			targets[i].entry, targets[i].sum = sources[t.Name].entry, sources[t.Name].sum
		}
		return targets, refs, nil
	}
	paired := clusternode.IsPaired(clusternode.PathsFromConfigDir(dir))
	if paired || role.RoleFromConfig(cfg.Cluster, paired) != role.RoleCoordinator {
		detail := "its node.yaml does not start it as a coordinator (cluster.mode " + string(cfg.Cluster.Mode) + ")"
		if paired {
			detail = "it is still paired with a cluster: " + filepath.Join(dir, "cluster") + " holds that coordinator's CA"
		}
		return nil, nil, &Refusal{NotCoordinator, detail, "A handoff makes this node a coordinator. On a worker, reset its pairing while it runs (zzrouter-node cluster pair --reset --cancel), stop it, set cluster.mode: coordinator in its node.yaml, then run this again with --replace."}
	}
	targets, err := handoffTargets(ctx, dir, sources)
	if err != nil {
		return nil, nil, err
	}
	var own []string
	if foreign {
		own = append(own, "its access store")
	}
	for _, t := range targets {
		if parts, _ := providerParts("config/" + t.Name); t.replaces() && (t.Name == "credentials.enc" || len(parts) > 1 && (parts[0] == "cloud" || parts[0] == "registries")) {
			own = append(own, t.Name)
		}
	}
	if len(own) > 0 && !replace {
		return nil, nil, &Refusal{ReplaceRequired, "the handoff would replace what this installation holds of its own: " + strings.Join(own, ", "),
			"Review what goes to the rollback directory with --dry-run --replace, then run this again with --replace."}
	}
	return targets, refs, nil
}

// handoffSources maps each file a handoff publishes, by its path in the
// installation in dir, to a target naming its staging entry and SHA-256: the
// carried configuration and, when the provider connections travel and the
// installation keeps its credential key at the default location, the exported
// key. An explicit key is read-only: it must already be the handoff's.
func handoffSources(dir string, credentials config.CredentialsConfig, m Manifest) (map[string]target, error) {
	sources := map[string]target{}
	for _, file := range m.Files {
		if name, ok := strings.CutPrefix(file.Name, "config/"); ok {
			sources[name] = target{Name: name, entry: file.Name, sum: file.SHA256}
		}
	}
	if _, travel := sources["credentials.enc"]; !travel {
		return sources, nil
	}
	keyPath := config.CredentialKeyPath(dir, credentials)
	if credentials.KeyFile == "" {
		rel, err := filepath.Rel(dir, keyPath)
		name := filepath.ToSlash(rel)
		sources[name] = target{Name: name, entry: keyEntry, sum: m.KeySHA256}
		return sources, err
	}
	key, err := secrets.LoadKeyFile(keyPath)
	defer clear(key)
	if err == nil && digest(key) == m.KeySHA256 {
		return sources, nil
	}
	detail := "the credential key_file " + keyPath + " is not the protection key the handoff's provider connections were saved with"
	if err != nil {
		detail += ": " + err.Error()
	}
	return nil, &Refusal{KeyMismatch, detail, "Explicit keys are read-only, so activation never writes one: place the key exported with the handoff at " + keyPath + ", owner-only, then run this again."}
}

// handoffTargets lists, sorted, what a handoff activation publishes into the
// installation in dir: every file sources names, every file the installation
// holds where a handoff carries one (handoffEntry), which goes unless sources
// names it, and every directory a new file needs; each file with the SHA-256
// of what it holds now. A link on a file's path is refused.
func handoffTargets(ctx context.Context, dir string, sources map[string]target) ([]target, error) {
	names := slices.Collect(maps.Keys(sources))
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		name := "config/" + filepath.ToSlash(rel)
		switch {
		case err != nil:
			return err
		case entry.IsDir() && !handoffDir(name):
			return fs.SkipDir
		case !entry.IsDir() && handoffEntry(name):
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	var targets []target
	made := map[string]bool{}
	for _, name := range slices.Compact(names) {
		t := sources[name]
		t.Name = name
		if _, t.Before, err = hashFile(ctx, dir, filepath.FromSlash(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for parent := path.Dir(name); t.Before == "" && parent != "."; parent = path.Dir(parent) {
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(parent))); !errors.Is(err, fs.ErrNotExist) {
				break
			}
			made[parent+"/"] = true
		}
		targets = append(targets, t)
	}
	for name := range made {
		targets = append(targets, target{Name: name})
	}
	slices.SortFunc(targets, func(a, b target) int { return strings.Compare(a.Name, b.Name) })
	return targets, nil
}

// references lists the node names of the staged configuration that named a
// machine of the source's cluster: route replicas pinned nowhere, which served
// on the source, or to the source or the destination, but not cloud ones,
// which any node serves; and provider entries nodes.<source> and
// nodes.<destination>.
func references(staging, source, destination string) ([]Reference, error) {
	invalid := func(err error) error {
		return &Refusal{StagingInvalid, "its configuration does not load here: " + err.Error(), "Take the handoff again from a source on this release, stage it into a new directory, and activate that one."}
	}
	apps, err := config.LoadAppsConfig(filepath.Join(staging, "config", "providers"))
	if err != nil {
		return nil, invalid(err)
	}
	groups := group.NewGroupStore()
	if err := groups.LoadFromFile(filepath.Join(staging, "config", "model_groups.yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, invalid(err)
	}
	meaning := map[string]string{"": "pinned nowhere: it served on the source, " + source, source: "the source", destination: "this node"}
	var refs []Reference
	routes := groups.List()
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		for _, r := range routes[name].Replicas {
			if apps.GetCloud(r.App) == nil && (r.Node == "" || r.Node == source || r.Node == destination) {
				refs = append(refs, Reference{Where: fmt.Sprintf("route %s, replica %s (%s)", name, r.Name, meaning[r.Node]), Node: r.Node,
					Fix: fmt.Sprintf(`zz_api {"method": "PATCH", "path": "/zzrouter/v1/model-groups/%s/replicas/%s", "body": {"node": "<node>"}}`, name, r.Name)})
			}
		}
	}
	names := apps.AppNames()
	slices.Sort(names)
	for _, name := range names {
		app, _ := apps.LookupApp(name)
		for _, node := range slices.Compact([]string{source, destination}) {
			if _, ok := app.Nodes[node]; ok {
				refs = append(refs, Reference{Where: fmt.Sprintf("provider %s, nodes.%s (%s)", name, node, meaning[node]), Node: node,
					Fix: fmt.Sprintf(`zz_params_set {"provider": %q, "patch": {"nodes": {%q: null}}}`, name, node)})
			}
		}
	}
	return refs, nil
}

// replaceFiles publishes targets, in order, from staging into the
// installation in dir: it makes each new directory; moves a file that is not
// the staged one yet into the rollback directory's files/, unless what it
// replaced is kept there already; then puts the staged copy in its place by
// rename, refusing a copy whose SHA-256 is not the checked one, or removes the
// file the handoff lacks. Each step is durable before the next and a target
// already holding the staged content is done, so a rerun after a crash
// completes the work. Last, it gives every target it may have written and
// what files/ holds the installation's owner. It returns the replaced targets
// files/ does not keep.
func replaceFiles(ctx context.Context, dir, staging, rollback string, targets []target) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	var lost, written []string
	incoming := filepath.Join(rollback, "incoming")
	for _, t := range targets {
		live, kept := filepath.Join(dir, filepath.FromSlash(t.Name)), filepath.Join(rollback, "files", filepath.FromSlash(t.Name))
		if t.isDir() || t.Before != t.sum {
			written = append(written, live)
		}
		if t.isDir() {
			if err := sqlitedb.MakeDir(ctx, live); err != nil {
				return nil, err
			}
			continue
		}
		_, now, err := hashFile(ctx, dir, filepath.FromSlash(t.Name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		has, err := exists(kept)
		switch {
		case err != nil:
			return nil, err
		case now == t.sum:
			if t.replaces() && !has {
				lost = append(lost, t.Name)
			}
			continue
		case now != "" && !has:
			if err := sqlitedb.MakeDir(ctx, filepath.Dir(kept)); err != nil {
				return nil, err
			}
			if err := sqlitedb.Rename(live, kept); err != nil {
				return nil, err
			}
			sqlitedb.Step(ctx, "kept "+t.Name)
		}
		if t.entry == "" {
			if err := errors.Join(removeFile(live), sqlitedb.SyncDir(filepath.Dir(live))); err != nil {
				return nil, err
			}
			sqlitedb.Step(ctx, "removed "+t.Name)
			continue
		}
		if err := removeFile(incoming); err != nil {
			return nil, err
		}
		sum, err := sqlitedb.CopyFile(ctx, filepath.Join(staging, filepath.FromSlash(t.entry)), incoming)
		if err == nil && sum != t.sum {
			err = fmt.Errorf("%s is not the one checked: its SHA-256 is %s, not %s", t.entry, sum, t.sum)
		}
		if err == nil {
			err = sqlitedb.Rename(incoming, live)
		}
		if err != nil {
			return nil, err
		}
		sqlitedb.Step(ctx, "published "+t.Name)
	}
	err := filepath.WalkDir(filepath.Join(rollback, "files"), func(path string, _ fs.DirEntry, err error) error {
		if err == nil {
			written = append(written, path)
		} else if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return lost, sqlitedb.GiveOwner(ctx, dir, written...)
}

// revertFiles undoes replaceFiles in reverse order: a file kept in the
// rollback directory's files/ returns by rename, a file the activation
// created goes, and a directory it made goes once empty. Before it changes
// anything, it refuses with keptLost when a file it replaced is neither as it
// was nor kept. Each step is durable before the next, so a rerun completes it.
func revertFiles(ctx context.Context, dir, rollback string, targets []target) error {
	kept := func(t target) string { return filepath.Join(rollback, "files", filepath.FromSlash(t.Name)) }
	for _, t := range targets {
		if t.Before == "" {
			continue
		}
		has, err := exists(kept(t))
		var now string
		if err == nil {
			_, now, err = hashFile(ctx, dir, filepath.FromSlash(t.Name))
		}
		switch {
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err
		case !has && now != t.Before:
			return keptLost(t.Name)
		}
	}
	for _, t := range slices.Backward(targets) {
		live := filepath.Join(dir, filepath.FromSlash(t.Name))
		has, err := exists(kept(t))
		switch {
		case err != nil:
		case t.isDir():
			err = sqlitedb.RemoveDirIfEmpty(live)
		case has:
			err = sqlitedb.Rename(kept(t), live)
		case t.Before == "":
			err = removeFile(live)
		default:
			continue
		}
		if err != nil {
			return err
		}
		sqlitedb.Step(ctx, "reverted "+t.Name)
	}
	return nil
}

// removeFile removes the file at path, if there is one, durably.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return sqlitedb.SyncDir(filepath.Dir(path))
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
