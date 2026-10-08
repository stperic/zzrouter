// Package apipath is the single source of truth for zzRouter's
// management API paths.
//
// Before this package the Go client restated every path as a string
// literal, 109 of them across internal/client and internal/cli, with
// nothing checking them against the routes the server actually
// registers. A server-side rename compiled cleanly and broke the CLI at
// runtime, on whichever command happened to be run next.
//
// Two things follow from putting them here:
//
//   - A rename is one edit, and the compiler finds every call site.
//   - TestAPIPaths_AllResolveToRegisteredRoutes walks the live gin
//     engine and fails when a path here matches no route, so the break
//     lands in CI instead of in someone's terminal.
//
// Query strings stay at call sites. A builder returns a path; what a
// caller appends to it is that caller's business, and threading every
// optional filter through here would trade one restatement for another.
package apipath

import (
	"net/url"
	"strings"
)

// Base prefixes every management route. The compat surfaces (/v1/* for
// OpenAI, /api/* for Ollama) are deliberately absent: their paths are
// fixed by someone else's contract, so a registry that invited renaming
// them would be a loaded gun.
const Base = "/zzrouter/v1"

// Collection and singleton paths.
const (
	Health        = Base + "/health"
	ServerVersion = Base + "/server/version"
	Search        = Base + "/search"
	SpendReport   = Base + "/spend/report"
	ServerRoutes  = Base + "/server/routes"

	Keys            = Base + "/keys"
	Teams           = Base + "/teams"
	Runs            = Base + "/runs"
	Jobs            = Base + "/jobs"
	Models          = Base + "/models"
	Nodes           = Base + "/nodes"
	Providers       = Base + "/providers"
	ProvidersStatus = Providers + "/status"
	Deployments     = Base + "/deployments"
	ModelGroups     = Base + "/model-groups"
	InferenceLogs   = Base + "/inference-logs"
	UsageModels     = Base + "/usage/models"

	ModelsDefaults = Models + "/defaults"
	ModelsShow     = Models + "/show"

	NodesCompatible = Nodes + "/compatible"

	ProvidersCatalog   = Providers + "/catalog"
	ProvidersInstances = Providers + "/instances"

	RunsLoad    = Runs + "/load"
	RunsPreview = Runs + "/preview"

	PricingOverrides = Base + "/pricing/overrides"
	PricingStatus    = Base + "/pricing/status"

	// Update routes act on the node the request reaches: they read that
	// node's scheduler and take no node selector.
	UpdateStatus   = Base + "/update/status"
	UpdateCheck    = Base + "/update/check"
	UpdateApply    = Base + "/update/apply"
	UpdateRollback = Base + "/update/rollback"
	UpdateHistory  = Base + "/update/history"

	ClusterPairingAccept = Base + "/cluster/pairing/accept"

	RegistriesHuggingFaceVariants = Base + "/registries/huggingface/variants"
)

// seg escapes one path segment. Identifiers reach these builders from
// user input (a model group name, a provider name), so a segment
// carrying a slash or a space must not be able to reshape the URL.
func seg(s string) string { return url.PathEscape(s) }

// join builds base/parts... with every part escaped.
func join(base string, parts ...string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(seg(p))
	}
	return b.String()
}

// Keys.
func Key(id string) string           { return join(Keys, id) }
func KeyRotate(id string) string     { return join(Keys, id, "rotate") }
func KeyUsage(id string) string      { return join(Keys, id, "usage") }
func KeyUsageReset(id string) string { return join(Keys, id, "usage", "reset") }

// Teams.
func Team(id string) string           { return join(Teams, id) }
func TeamKeys(id string) string       { return join(Teams, id, "keys") }
func TeamUsage(id string) string      { return join(Teams, id, "usage") }
func TeamUsageReset(id string) string { return join(Teams, id, "usage", "reset") }

// Runs.
func Run(id string) string     { return join(Runs, id) }
func RunLogs(id string) string { return join(Runs, id, "logs") }

// Jobs.
func Job(id string) string       { return join(Jobs, id) }
func JobStream(id string) string { return join(Jobs, id, "stream") }

// Nodes.
func Node(name string) string { return join(Nodes, name) }

// Deployments.
func Deployment(id string) string           { return join(Deployments, id) }
func DeploymentNode(id, node string) string { return join(Deployments, id, "nodes", node) }

// Model groups, inference logs, usage.
func ModelGroup(name string) string        { return join(ModelGroups, name) }
func InferenceLog(id string) string        { return join(InferenceLogs, id) }
func InferenceLogPayload(id string) string { return join(InferenceLogs, id, "payload") }
func UsageModel(model string) string       { return join(UsageModels, model) }

// ModelCard does NOT escape its trailing segment. The route is a
// catch-all (*id) and HuggingFace IDs carry slashes ("org/model") that
// have to survive as path separators, not become %2F.
func ModelCard(provider, id string) string { return join(Models, "card", provider) + "/" + id }

// Cluster.
//
// clusterEndpoints is a prefix, not a route: only DELETE
// .../endpoints/{address} is registered, so exporting it would advertise
// a collection endpoint that 404s. Same reason DeploymentNode has no
// DeploymentNodes sibling. The address key is deliberate: the resource
// is a config row in node.yaml, keyed by exact host:port, distinct from
// the runtime node /nodes/:name serves.
const clusterEndpoints = Base + "/cluster/endpoints"

func ClusterEndpoint(address string) string { return join(clusterEndpoints, address) }
func RunProbes(id string) string            { return join(Runs, id, "probes") }

// Provider lifecycle.
func ProviderVersionsFor(n string) string   { return join(Providers, n, "versions") }
func ProviderSchema(n string) string        { return join(Providers, n, "schema") }
func ProviderResolved(n string) string      { return join(Providers, n, "resolved") }
func ProviderAssets(n string) string        { return join(Providers, n, "assets") }
func ProviderAsset(n, a string) string      { return join(Providers, n, "assets", a) }
func ProviderServiceStatus(n string) string { return join(Providers, n, "service", "status") }
func ProviderServiceApply(n string) string  { return join(Providers, n, "service", "apply") }
func ProviderUpgrade(n string) string       { return join(Providers, n, "upgrade") }
func ProviderVerify(n string) string        { return join(Providers, n, "verify") }
func ProviderInstall(n string) string       { return join(Providers, n, "install") }
func ProviderInstallPlan(n string) string   { return join(Providers, n, "install", "plan") }
func ProviderInstallPreflight(n string) string {
	return join(Providers, n, "install", "preflight")
}
func ProviderInstallVerifyStep(n string) string {
	return join(Providers, n, "install", "verify-step")
}
func ProviderInstallExecuteStep(n string) string {
	return join(Providers, n, "install", "execute-step")
}

// Providers.
func Provider(name string) string { return join(Providers, name) }

// Provider parameters: three tiers, one shape.
func ProviderParameters(n string) string     { return join(Providers, n, "parameters") }
func ProviderParameter(n, key string) string { return join(Providers, n, "parameters", key) }
func ProviderParameterIgnore(n, key string) string {
	return join(Providers, n, "parameters", key, "ignore")
}
func ProviderNodesParameters(n string) string { return join(Providers, n, "nodes", "parameters") }
func ProviderNodeParameter(n, key string) string {
	return join(Providers, n, "nodes", "parameters", key)
}
func ProviderNodeParameterIgnore(n, key string) string {
	return join(Providers, n, "nodes", "parameters", key, "ignore")
}
func ProviderModelsParameters(n string) string { return join(Providers, n, "models", "parameters") }
func ProviderModelParameter(n, key string) string {
	return join(Providers, n, "models", "parameters", key)
}
func ProviderModelParameterIgnore(n, key string) string {
	return join(Providers, n, "models", "parameters", key, "ignore")
}
