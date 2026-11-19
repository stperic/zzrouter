package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stretchr/testify/require"
)

// The source coordinator's node name, as recoveryFixture writes it, and the
// destination's.
const sourceNode, destinationNode = "recovery-test", "dest-1"

// handoffOptions is an activation of a handoff on a destination that holds
// its own store.
var handoffOptions = ActivateOptions{SourceStopped: true, Replace: true}

// handoffActivationFixture stages a handoff of handoffInstallation's
// coordinator, whose routes and llama.cpp definition name it, the destination
// and no node, and whose spend holds a key its store lacks. Its destination is
// recoveryFixture's coordinator named dest-1, with a connection, a key and a
// store of its own, an edited cloud definition, a cloud provider the source
// lacks, static keys, a CA and an identity. It returns the destination, the
// staging directory, its manifest and the issued references of the source.
func handoffActivationFixture(t *testing.T) (dir, staging string, manifest Manifest, before issued) {
	t.Helper()
	source, output, keyOutput, _ := handoffInstallation(t)
	before = issue(t, source)
	groups := group.NewGroupStore()
	require.NoError(t, groups.Set("assistant", group.ModelGroup{Replicas: []group.Replica{
		{Name: "local", Model: "qwen", App: "llamacpp"}, {Name: "old", Model: "qwen", App: "llamacpp", Node: sourceNode, Priority: 2},
		{Name: "here", Model: "qwen", App: "llamacpp", Node: destinationNode, Priority: 3}, {Name: "gpu", Model: "qwen", App: "llamacpp", Node: "gpu-1", Priority: 4},
	}}))
	require.NoError(t, groups.Set("frontier", group.ModelGroup{Replicas: []group.Replica{{Name: "openai", Model: "gpt-5", App: "openai"}}}))
	require.NoError(t, groups.SaveToFile(filepath.Join(source, "model_groups.yaml")))
	definition := filepath.Join(source, "providers", "on-demand", "llamacpp", "config.yaml")
	data := readBytes(t, definition)
	require.NoError(t, os.WriteFile(definition, append(data, []byte("nodes:\n  "+sourceNode+":\n    enabled: false\n  "+destinationNode+":\n    enabled: true\n  gpu-1:\n    enabled: true\n")...), 0o600))
	spend, err := quota.LoadSpend(filepath.Join(source, "spend.json"))
	require.NoError(t, err)
	spend.States["key:ghost:"] = &quota.SpendState{Kind: quota.ScopeKey, EntityID: "ghost", SpendMicro: 9}
	require.NoError(t, quota.SaveSpend(filepath.Join(source, "spend.json"), spend))
	_, err = BackupProfile(t.Context(), ProfileHandoff, source, output, keyOutput)
	require.NoError(t, err)
	staging = filepath.Join(t.TempDir(), "staged")
	manifest, err = Restore(t.Context(), output, keyOutput, staging)
	require.NoError(t, err)

	dir, _, _ = recoveryFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte("node:\n  name: "+destinationNode+"\n  port: 9090\n"), 0o600))
	key, err := config.PrepareCredentialKey(t.Context(), dir, config.CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(dir, "credentials.enc"), key)
	require.NoError(t, err)
	_, err = vault.Replace(t.Context(), "cloud", 1, map[string]string{"token": "destination-secret"})
	require.NoError(t, errors.Join(err, vault.Close()))
	openai := filepath.Join(dir, "providers", "cloud", "openai", "config.yaml")
	require.NoError(t, os.WriteFile(openai, append(readBytes(t, openai), []byte("# the destination's own edit\n")...), 0o600))
	copyProvider(t, dir, "cloud/groq", "cloud/mine")
	for _, name := range []string{".env", "ca/ca.key", "ca/ca.pem", "identity/node.key", "identity/node.pem"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("the destination's own "+name), 0o600))
	}
	return dir, staging, manifest, before
}

// relTree maps every path under dir, relative to it, to its content's digest,
// "/" for a directory; skip names paths left out.
func relTree(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for path, content := range tree(t, dir) {
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		if rel = filepath.ToSlash(rel); !slices.ContainsFunc(skip, func(s string) bool { return strings.HasPrefix(rel, s) }) {
			out[rel] = content
		}
	}
	return out
}

// wantReferences is what the fixture's staged configuration names.
func wantReferences() []Reference {
	route := func(replica, node, meaning string) Reference {
		return Reference{Where: "route assistant, replica " + replica + " (" + meaning + ")", Node: node,
			Fix: `zz_api {"method": "PATCH", "path": "/zzrouter/v1/model-groups/assistant/replicas/` + replica + `", "body": {"node": "<node>"}}`}
	}
	provider := func(node, meaning string) Reference {
		return Reference{Where: "provider llamacpp, nodes." + node + " (" + meaning + ")", Node: node, Fix: `zz_params_set {"provider": "llamacpp", "patch": {"nodes": {"` + node + `": null}}}`}
	}
	return []Reference{route("local", "", "pinned nowhere: it served on the source, "+sourceNode), route("old", sourceNode, "the source"), route("here", destinationNode, "this node"),
		provider(sourceNode, "the source"), provider(destinationNode, "this node")}
}

// requireHandoffActivated requires dir to hold what staging carried, every
// file the destination had where a handoff carries one but the handoff lacks
// kept in the rollback directory and gone, and what a handoff never touches
// as it was in untouched.
func requireHandoffActivated(t *testing.T, dir string, manifest Manifest, untouched map[string]string) {
	t.Helper()
	require.NoError(t, PendingActivation(dir))
	live := relTree(t, dir)
	for _, file := range manifest.Files {
		if name, ok := strings.CutPrefix(file.Name, "config/"); ok {
			require.Equal(t, file.SHA256, live[name], "%s is the carried file", name)
		}
	}
	require.Equal(t, manifest.KeySHA256, live["private/credentials.key"], "the exported key is in place")
	require.Equal(t, manifest.Staged.ImageSHA256, live["access/access.sqlite"])
	require.NotContains(t, live, "providers/cloud/mine/config.yaml", "a definition the handoff lacks goes")
	rollback := filepath.Join(dir, rollbackDir, manifest.Staged.Generation)
	require.FileExists(t, filepath.Join(rollback, "files", "providers", "cloud", "mine", "config.yaml"), "and is kept")
	for name, content := range untouched {
		require.Equal(t, content, live[name], "%s is the destination's own", name)
	}
}

// untouchedOf is what no activation changes in dir: node.yaml, static keys,
// CA, identity, pairing, the recovery layout and the lock.
func untouchedOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, content := range relTree(t, dir) {
		if slices.Contains([]string{"node.yaml", ".env", "ca/ca.key", "ca/ca.pem", "identity/node.key", "identity/node.pem", "pairing.txt", InventoryFile, "installation.lock", "private/credentials.lock"}, name) {
			out[name] = content
		}
	}
	require.Len(t, out, 10)
	return out
}

// TestHandoffActivationPublishesTheSourceCoordinator: a dry run writes nothing
// and reports what the activation then reports; the activation publishes the
// source's store, configuration and key, keeps what it replaced, rewrites no
// node name and leaves node.yaml, static keys, CA and identity as they were.
// At the first start the owner signs in with the old password and a raw app
// key works while sessions and app-key references from before do not, the
// connections decrypt with the placed key, routes and spend are the source's,
// and the spend of a key the image lacks is gone.
func TestHandoffActivationPublishesTheSourceCoordinator(t *testing.T) {
	dir, staging, manifest, before := handoffActivationFixture(t)
	installation, staged, untouched := tree(t, dir), tree(t, staging), untouchedOf(t, dir)
	routes := readBytes(t, filepath.Join(staging, "config", "model_groups.yaml"))
	dry, err := Activate(t.Context(), dir, staging, ActivateOptions{DryRun: true, SourceStopped: true, Replace: true})
	require.NoError(t, err)
	require.Equal(t, installation, tree(t, dir), "a dry run writes nothing")
	require.Equal(t, staged, tree(t, staging))
	require.Equal(t, wantReferences(), dry.References)
	require.Equal(t, []string{"store", "credentials.enc", "private/credentials.key", "providers/cloud/mine/config.yaml", "providers/cloud/openai/config.yaml", "providers/on-demand/llamacpp/config.yaml", "spend.json"}, dry.Replaced)

	a, err := Activate(t.Context(), dir, staging, handoffOptions)
	require.NoError(t, err)
	require.Equal(t, dry.References, a.References, "the activation reports what the dry run did")
	require.Equal(t, dry.Replaced, a.Replaced)
	require.Empty(t, a.Lost)
	require.NoDirExists(t, staging)
	requireHandoffActivated(t, dir, manifest, untouched)
	require.Equal(t, routes, readBytes(t, filepath.Join(dir, "model_groups.yaml")), "D6: no node name is rewritten")
	apps, err := config.LoadAppsConfig(filepath.Join(dir, "providers"))
	require.NoError(t, err, "the provider tree loads")
	llamacpp, _ := apps.LookupApp("llamacpp")
	require.Contains(t, llamacpp.Nodes, sourceNode)

	// The first start.
	store, err := sqlstore.Open(t.Context(), dir, peopleOptions)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.Equal(t, [2]string{manifest.Store.StoreID, manifest.Staged.Generation}, [2]string{store.Meta().StoreID, store.Meta().Generation})
	_, err = store.People().Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err, "the owner signs in with the old password")
	spendPath := filepath.Join(dir, "spend.json")
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(spendPath))
	core := control.NewCore(control.StaticKeySet{}, store, nil, enforcer, nil)
	t.Cleanup(func() { core.Stop(context.Background()) })
	_, err = core.Reference(before.raw)
	require.NoError(t, err, "the raw app key authenticates")
	_, err = core.AuthenticateReference(before.session)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an old session does not resolve")
	_, err = core.AuthenticateReference(before.app)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an old app-key reference does not resolve")
	key, err := config.PrepareCredentialKey(t.Context(), dir, config.CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(dir, "credentials.enc"), key)
	require.NoError(t, err)
	record, err := vault.Snapshot("cloud")
	require.NoError(t, errors.Join(err, vault.Close()))
	require.Equal(t, "synthetic-provider-secret", record.Values["token"], "the source's connections decrypt")
	groups := group.NewGroupStore()
	require.NoError(t, groups.LoadFromFile(filepath.Join(dir, "model_groups.yaml")))
	require.NotNil(t, groups.Get("assistant"))
	spend, err := quota.LoadSpend(spendPath)
	require.NoError(t, err)
	require.EqualValues(t, 1750000, spend.States["team:workspace:"].SpendMicro, "the spend totals are the source's")
	require.NotContains(t, spend.States, "key:ghost:", "the spend of a key the image lacks is gone")
}

// TestHandoffActivationRefusalsWriteNothing: each refusal of a handoff names
// its closed reason and next action, and leaves the installation and the
// staging directory byte-identical, in a dry run too.
func TestHandoffActivationRefusalsWriteNothing(t *testing.T) {
	// fresh removes the destination's store, connections and own cloud
	// definitions, so only what a case adds is its own.
	fresh := func(t *testing.T, dir, staging string) {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")))
		require.NoError(t, os.Remove(filepath.Join(dir, sqlstore.MarkerName)))
		require.NoError(t, os.Remove(filepath.Join(dir, "credentials.enc")))
		require.NoError(t, os.RemoveAll(filepath.Join(dir, "providers", "cloud", "mine")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "providers", "cloud", "openai", "config.yaml"), readBytes(t, filepath.Join(staging, "config", "providers", "cloud", "openai", "config.yaml")), 0o600))
	}
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, dir, staging string)
		options ActivateOptions
		reason  Reason
		detail  string
	}{
		"no --source-stopped": {options: ActivateOptions{Replace: true}, reason: SourceNotStopped, detail: "still runs"},
		"a worker": {prepare: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte("node:\n  name: "+destinationNode+"\n  port: 9090\ncluster:\n  mode: worker\n"), 0o600))
		}, reason: NotCoordinator, detail: "cluster.mode worker"},
		"a paired coordinator": {prepare: func(t *testing.T, dir, _ string) { pair(t, dir) }, reason: NotCoordinator, detail: "still paired"},
		"its own store": {prepare: func(t *testing.T, dir, staging string) {
			fresh(t, dir, staging)
			store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, errors.Join(err, store.Close()))
		}, options: ActivateOptions{SourceStopped: true}, reason: ReplaceRequired, detail: "of its own: its access store"},
		"its own connections": {prepare: func(t *testing.T, dir, staging string) {
			fresh(t, dir, staging)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.enc"), []byte("the destination's own"), 0o600))
		}, options: ActivateOptions{SourceStopped: true}, reason: ReplaceRequired, detail: "of its own: credentials.enc"},
		"its own cloud definition": {prepare: func(t *testing.T, dir, staging string) {
			fresh(t, dir, staging)
			copyProvider(t, dir, "cloud/groq", "cloud/mine")
		}, options: ActivateOptions{SourceStopped: true}, reason: ReplaceRequired, detail: "of its own: providers/cloud/mine/config.yaml"},
		"its own registry definition": {prepare: func(t *testing.T, dir, staging string) {
			fresh(t, dir, staging)
			path := filepath.Join(dir, "providers", "registries", "huggingface", "config.yaml")
			require.NoError(t, os.WriteFile(path, append(readBytes(t, path), '\n'), 0o600))
		}, options: ActivateOptions{SourceStopped: true}, reason: ReplaceRequired, detail: "of its own: providers/registries/huggingface/config.yaml"},
		"an explicit key that differs": {prepare: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte("node:\n  name: "+destinationNode+"\n  port: 9090\nsecurity:\n  credentials:\n    key_file: private/credentials.key\n"), 0o600))
		}, reason: KeyMismatch, detail: "is not the protection key"},
		"no staged key": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.RemoveAll(filepath.Join(staging, "protection")))
		}, reason: StagingInvalid, detail: "does not hold " + keyEntry},
		"a staged key that differs": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.WriteFile(filepath.Join(staging, filepath.FromSlash(keyEntry)), make([]byte, 32), 0o600))
		}, reason: StagingInvalid, detail: "SHA-256 of " + keyEntry},
		"an altered definition": {prepare: func(t *testing.T, _, staging string) {
			path := filepath.Join(staging, "config", "providers", "on-demand", "llamacpp", "config.yaml")
			require.NoError(t, os.WriteFile(path, append(readBytes(t, path), '\n'), 0o600))
		}, reason: StagingInvalid, detail: "SHA-256 of config/providers/on-demand/llamacpp/config.yaml"},
		"an extra staged file": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.WriteFile(filepath.Join(staging, "config", "node.yaml"), nil, 0o600))
		}, reason: StagingInvalid, detail: "entries other than"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, staging, _, _ := handoffActivationFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, dir, staging)
			}
			if tc.options == (ActivateOptions{}) {
				tc.options = handoffOptions
			}
			installation, staged := tree(t, dir), tree(t, staging)
			for _, dry := range []bool{true, false} {
				o := tc.options
				o.DryRun = dry
				_, err := Activate(t.Context(), dir, staging, o)
				require.Contains(t, requireRefusal(t, err, tc.reason).Detail, tc.detail)
				require.Equal(t, installation, tree(t, dir), "the installation is unchanged")
				require.Equal(t, staged, tree(t, staging), "the staging directory is unchanged")
			}
		})
	}
}

// pair gives dir a worker's pairing with a cluster: its CA, coordinator URL
// and an identity that CA signed.
func pair(t *testing.T, dir string) {
	t.Helper()
	ca, err := clusterid.LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "identity")))
	identity, err := clusterid.LoadOrCreateIdentity(filepath.Join(dir, "identity"))
	require.NoError(t, err)
	csr, err := identity.CSR(destinationNode, nil, nil)
	require.NoError(t, err)
	signed, err := ca.Sign(csr, clusterid.RoleWorker)
	require.NoError(t, err)
	require.NoError(t, identity.InstallSignedCert(signed))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cluster"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cluster", "ca.pem"), ca.CertPEM(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cluster", "coordinator_url"), []byte("https://old-coordinator:9091"), 0o600))
}

// TestHandoffActivationOnAWorker (D8): a paired worker is refused; reset, in
// coordinator mode and with --replace, it takes the source's provider tree in
// place of its cached on-demand and external files, and its own cloud and
// registry definitions and connections go to the rollback directory.
func TestHandoffActivationOnAWorker(t *testing.T) {
	dir, staging, manifest, _ := handoffActivationFixture(t)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")), "a worker has no store")
	require.NoError(t, os.Remove(filepath.Join(dir, sqlstore.MarkerName)))
	worker := []byte("node:\n  name: " + destinationNode + "\n  port: 9090\ncluster:\n  mode: worker\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), worker, 0o600))
	cached := filepath.Join(dir, "providers", "on-demand", "vllm", "config.yaml")
	require.NoError(t, os.WriteFile(cached, append(readBytes(t, cached), []byte("# a stale cache of another coordinator's tree\n")...), 0o600))
	pair(t, dir)
	installation := tree(t, dir)
	_, err := Activate(t.Context(), dir, staging, handoffOptions)
	requireRefusal(t, err, NotCoordinator)
	require.Equal(t, installation, tree(t, dir))

	// zzrouter-node cluster pair --reset --cancel removes cluster/; then the
	// operator stops the node and sets coordinator mode.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "cluster")))
	_, err = Activate(t.Context(), dir, staging, handoffOptions)
	requireRefusal(t, err, NotCoordinator)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(strings.Replace(string(worker), "worker", "coordinator", 1)), 0o600))
	_, err = Activate(t.Context(), dir, staging, ActivateOptions{SourceStopped: true})
	require.Contains(t, requireRefusal(t, err, ReplaceRequired).Detail, "credentials.enc, providers/cloud/mine/config.yaml, providers/cloud/openai/config.yaml")
	untouched := untouchedOf(t, dir)
	a, err := Activate(t.Context(), dir, staging, handoffOptions)
	require.NoError(t, err)
	require.Equal(t, []string{"credentials.enc", "private/credentials.key", "providers/cloud/mine/config.yaml", "providers/cloud/openai/config.yaml", "providers/on-demand/llamacpp/config.yaml", "providers/on-demand/vllm/config.yaml", "spend.json"}, a.Replaced)
	requireHandoffActivated(t, dir, manifest, untouched)
	kept := filepath.Join(a.RollbackDir, "files")
	for _, name := range []string{"credentials.enc", "providers/cloud/openai/config.yaml", "providers/on-demand/vllm/config.yaml"} {
		require.FileExists(t, filepath.Join(kept, filepath.FromSlash(name)), "the worker's own %s is kept", name)
	}
}

// TestHandoffActivationSurvivesAKillAtEveryStep kills a real process at each
// durable step of a handoff activation, every target included. Until the
// activation marker goes, a start refuses with activation_incomplete; a
// rollback then restores the whole installation byte for byte: files,
// directories, the store and the key; and a rerun instead completes the
// activation. Killed at each step of the deepest rollback, a rerun of the
// rollback completes it, and so does an activation run instead. It forks, so
// it does not run in parallel (see TestActivationSurvivesAKillAtEveryStep).
func TestHandoffActivationSurvivesAKillAtEveryStep(t *testing.T) {
	dir, staging, manifest, _ := handoffActivationFixture(t)
	original, untouched := relTree(t, dir), untouchedOf(t, dir)
	activated := copyTree(t, dir)
	_, err := Activate(t.Context(), activated, copyTree(t, staging), handoffOptions)
	require.NoError(t, err)
	record := filepath.ToSlash(filepath.Join(rollbackDir, manifest.Staged.Generation, rollbackFile))
	want := relTree(t, activated, record)
	steps := stepsOf(t, dir, copyTree(t, staging), false)
	for _, step := range []string{"published providers/cloud/openai/assets/chat-template.jinja", "made assets", "kept providers/cloud/mine/config.yaml", "removed providers/cloud/mine/config.yaml", "kept private/credentials.key", "published private/credentials.key", "published model_groups.yaml"} {
		require.Contains(t, steps, step)
	}
	for k, step := range steps {
		t.Run(step, func(t *testing.T) {
			killed, staged := copyTree(t, dir), copyTree(t, staging)
			require.Equal(t, step, killAt(t, killed, staged, k+1, false))
			if k == len(steps)-1 {
				require.NoError(t, PendingActivation(killed), "the activation finished")
				require.Equal(t, want, relTree(t, killed, record))
				return
			}
			requireRefusal(t, PendingActivation(killed), ActivationIncomplete)
			undone := copyTree(t, killed)
			_, err := Activate(t.Context(), undone, staged, ActivateOptions{Rollback: true})
			require.NoError(t, err)
			require.Equal(t, original, relTree(t, undone), "the rollback restores the installation byte for byte")
			_, err = Activate(t.Context(), killed, staged, ActivateOptions{})
			require.NoError(t, err, "a rerun completes the activation")
			require.Equal(t, want, relTree(t, killed, record))
			requireHandoffActivated(t, killed, manifest, untouched)
		})
	}

	replaced := copyTree(t, dir)
	require.Equal(t, steps[len(steps)-2], killAt(t, replaced, staging, len(steps)-1, false))
	undo := stepsOf(t, replaced, staging, true)
	require.Contains(t, undo, "reverted providers/cloud/mine/config.yaml")
	for j, step := range undo {
		t.Run("rollback/"+step, func(t *testing.T) {
			killed, staged := copyTree(t, dir), copyTree(t, staging)
			killAt(t, killed, staged, len(steps)-1, false)
			require.Equal(t, step, killAt(t, killed, staged, j+1, true))
			undone := copyTree(t, killed)
			_, err := Activate(t.Context(), undone, staged, ActivateOptions{Rollback: true})
			if j == len(undo)-1 {
				requireRefusal(t, err, NoActivation)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, original, relTree(t, undone))
			_, err = Activate(t.Context(), killed, staged, ActivateOptions{SourceStopped: true, Replace: true})
			require.NoError(t, err, "an activation run instead completes")
			require.Equal(t, want, relTree(t, killed, record))
		})
	}
}

// TestHandoffRollbackRefusesWithoutWhatItKept: with the rollback directory's
// copy of a replaced file deleted, the rollback refuses, changing nothing, and
// points at finishing, which then reports the file as not kept.
func TestHandoffRollbackRefusesWithoutWhatItKept(t *testing.T) {
	t.Parallel()
	dir, staging, manifest, _ := handoffActivationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := Activate(sqlitedb.WithStep(ctx, func(step string) {
		if step == "published providers/cloud/openai/config.yaml" {
			cancel()
		}
	}), dir, staging, handoffOptions)
	require.ErrorIs(t, err, context.Canceled)
	kept := filepath.Join(dir, rollbackDir, manifest.Staged.Generation, "files", "providers", "cloud", "openai", "config.yaml")
	require.NoError(t, os.Remove(kept))
	installation := tree(t, dir)
	_, err = Activate(t.Context(), dir, staging, ActivateOptions{Rollback: true})
	refusal := requireRefusal(t, err, ActivationIncomplete)
	require.Contains(t, refusal.Detail, "providers/cloud/openai/config.yaml was already replaced")
	require.Contains(t, refusal.NextAction, "to finish it")
	require.Equal(t, installation, tree(t, dir), "nothing changed")
	a, err := Activate(t.Context(), dir, staging, ActivateOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"providers/cloud/openai/config.yaml"}, a.Lost)
	require.NotContains(t, a.Replaced, "providers/cloud/openai/config.yaml")
}

// TestHandoffActivationRecordsItsTargets: the activation marker names every
// target with what it held, so a rollback needs nothing but the rollback
// directory: interrupted once it created an asset and its directory, the
// rollback without a staging directory removes both and restores the rest.
func TestHandoffActivationRecordsItsTargets(t *testing.T) {
	t.Parallel()
	dir, staging, _, _ := handoffActivationFixture(t)
	original := relTree(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := Activate(sqlitedb.WithStep(ctx, func(step string) {
		if step == "published providers/cloud/openai/assets/chat-template.jinja" {
			cancel()
		}
	}), dir, staging, handoffOptions)
	require.ErrorIs(t, err, context.Canceled)
	require.FileExists(t, filepath.Join(dir, "providers", "cloud", "openai", "assets", "chat-template.jinja"))
	var record activation
	require.NoError(t, json.Unmarshal(readBytes(t, filepath.Join(dir, activationFile)), &record))
	byName := map[string]string{}
	for _, target := range record.Targets {
		byName[target.Name] = target.Before
	}
	require.Equal(t, original["providers/cloud/mine/config.yaml"], byName["providers/cloud/mine/config.yaml"])
	require.Contains(t, byName, "model_groups.yaml")
	require.Empty(t, byName["model_groups.yaml"], "absent before")
	require.Contains(t, byName, "providers/cloud/openai/assets/", "a directory a new file needs")
	require.NoError(t, os.RemoveAll(staging))
	_, err = Activate(t.Context(), dir, staging, ActivateOptions{Rollback: true})
	require.NoError(t, err, "a rollback needs no staging directory")
	_, err = os.Stat(filepath.Join(dir, "providers", "cloud", "openai", "assets"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Equal(t, original, relTree(t, dir))
}

// TestHandoffActivationPublishesTheCheckedBytes: a staged file swapped after
// the checks is refused before it is published, the activation stays
// incomplete, and a rollback restores the installation byte for byte.
func TestHandoffActivationPublishesTheCheckedBytes(t *testing.T) {
	t.Parallel()
	dir, staging, _, _ := handoffActivationFixture(t)
	original := relTree(t, dir)
	definition := filepath.Join(staging, "config", "providers", "on-demand", "llamacpp", "config.yaml")
	_, err := Activate(sqlitedb.WithStep(t.Context(), func(step string) {
		if step == "rollback record" {
			require.NoError(t, os.WriteFile(definition, []byte("enabled: true\n"), 0o600))
		}
	}), dir, staging, handoffOptions)
	require.ErrorContains(t, err, "is not the one checked")
	requireRefusal(t, err, ActivationIncomplete)
	published, _ := os.ReadFile(filepath.Join(dir, "providers", "on-demand", "llamacpp", "config.yaml"))
	require.NotEqual(t, "enabled: true\n", string(published), "the swapped file is not published")
	_, err = Activate(t.Context(), dir, staging, ActivateOptions{Rollback: true})
	require.NoError(t, err)
	require.Equal(t, original, relTree(t, dir))
}
