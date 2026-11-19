package recovery

import (
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
)

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
	rest, ok := strings.CutPrefix(name, "config/providers/")
	parts := strings.Split(rest, "/") // <kind>/<name>/<file> or <kind>/<name>/assets/<asset>
	if !ok || len(parts) < 3 || !config.IsProviderKindDir(parts[0]) {
		return false
	}
	switch len(parts) {
	case 3:
		return parts[2] == "config.yaml" || parts[2] == "schema.yaml"
	case 4:
		return parts[2] == assets.DirName && assets.ValidateName(parts[3]) == nil
	}
	return false
}

// handoffSeparate is what the operator of a handoff moves or recreates by
// hand, since no handoff carries it.
func handoffSeparate() []string {
	return []string{
		"The destination's own node.yaml: node name, listen address, TLS, storage paths, authentication and cluster role.",
		"Static installation keys (ZZROUTER_ADMIN_API_KEY, ZZROUTER_API_KEY) from node.yaml or a dotenv file: set them on the destination if clients still use them.",
		"External TLS or proxy trust material that node.yaml names: copy it over a secure channel and name it in the destination's node.yaml.",
		"Provider runtimes and model weights: install and download them on the destination.",
		"Workers: pair each one again with the new coordinator. The cluster CA, node identity and worker enrollments stay with the old one.",
	}
}
