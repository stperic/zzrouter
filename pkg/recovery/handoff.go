package recovery

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
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
