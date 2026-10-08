package server

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

const apiDiscoveryPrefix = "/zzrouter/v1"

// apiDiscoveryHidden — routes kept out of the agent-facing discovery
// doc: the raw route catalog (tooling, not a workflow) and the e2e
// harness state gates. Other /server/* routes list normally.
var apiDiscoveryHidden = map[string]struct{}{
	apiDiscoveryPrefix + "/server/routes":  {},
	apiDiscoveryPrefix + "/state/snapshot": {},
	apiDiscoveryPrefix + "/state/restore":  {},
}

type apiDiscoveryResponse struct {
	APIVersion          string              `json:"api_version"`
	Authentication      apiAuthentication   `json:"authentication"`
	Inference           apiInference        `json:"inference"`
	EndpointGroups      map[string][]string `json:"endpoint_groups"`
	StreamingGETs       []string            `json:"streaming_gets"`
	EnabledFeatures     []string            `json:"enabled_features"`
	Docs                map[string]string   `json:"docs"`
	ProviderDiagnostics string              `json:"provider_diagnostics,omitempty"`
	UpdateControl       string              `json:"update_control"`
}

// streamingGETs are the GET routes that answer text/event-stream and hold
// the connection open until the client hangs up.
//
// endpoint_groups lists them beside ordinary reads, so an agent walking
// the index to see what a node offers blocks on them with no warning.
// Naming them is the difference between "this endpoint is slow" and "this
// endpoint is a subscription".
//
// Endpoints that stream only on request (`/logs?follow=true`, a jobs
// stream negotiated with Accept) are not listed: a caller that did not ask
// for a stream will not get one. Pinned to the route table by
// TestAPIDiscovery_StreamingGETsAreRegistered.
var streamingGETs = []string{
	"GET " + apiDiscoveryPrefix + "/jobs/{id}/stream",
	"GET " + apiDiscoveryPrefix + "/model-groups/events",
	"GET " + apiDiscoveryPrefix + "/spend/events",
}

// apiInference addresses the inference surface. endpoint_groups indexes
// the management API only, so a caller that followed the discovery link
// from GET / could enumerate 150 endpoints without finding the one it
// came for.
//
// Endpoints is keyed by the same vocabulary /v1/models publishes per
// model, which is what turns a catalog entry into a request: an entry
// carrying endpoints:["chat"] is served at the path Endpoints["chat"]
// names. Values are pinned against the route table by
// TestAPIDiscovery_InferenceEndpointsAreRegistered.
type apiInference struct {
	// ServedHere is false on a worker, whose compat surface lives on the
	// cluster mTLS port rather than this one. The remaining fields are
	// empty in that case rather than naming paths this port will 404.
	ServedHere   bool              `json:"served_on_this_port"`
	Catalog      string            `json:"catalog,omitempty"`
	Endpoints    map[string]string `json:"endpoints,omitempty"`
	OllamaCompat string            `json:"ollama_compatible_base_path,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// inferenceEndpointPaths maps the model catalog's endpoint vocabulary
// onto the routes that serve it.
var inferenceEndpointPaths = map[string]string{
	"chat":        "POST /v1/chat/completions",
	"completions": "POST /v1/completions",
	"embeddings":  "POST /v1/embeddings",
	"rerank":      "POST /v1/rerank",
	"responses":   "POST /v1/responses",
	"messages":    "POST /v1/messages",
}

// describeInference reports the inference surface as reachable FROM THE
// PORT THIS DOCUMENT WAS FETCHED FROM.
//
// A worker serves /v1/* and /api/* on the cluster mTLS port, so naming
// those paths here would point a caller at an address this port answers
// 404 for — the same lie describeCompatAuth already refuses to tell.
// Endpoints is nil in that case: the block still states where the
// surface lives, and a caller iterating it gets nothing rather than
// something wrong.
//
// The maps are rebuilt per call rather than shared: this value is handed
// straight to the JSON encoder, and package-level state reachable from a
// response is a mutation hazard waiting for its first careless caller.
func describeInference(s *Server) apiInference {
	if s.config != nil && s.config.Cluster.IsWorker() {
		return apiInference{
			ServedHere: false,
			Note: "not served on this port: a worker exposes /v1/* and /api/* on the cluster mTLS port, " +
				"reachable only through its coordinator",
		}
	}
	return apiInference{
		ServedHere:   true,
		Catalog:      "GET /v1/models",
		Endpoints:    maps.Clone(inferenceEndpointPaths),
		OllamaCompat: "/api",
		Note: "send the model id exactly as /v1/models publishes it; " +
			"an \"@node\" suffix pins the request to that node",
	}
}

// apiAuthentication states how to present a key and which surfaces
// demand one. Discovery is the document a caller reads before it has
// made a request, so this is the only place the accepted schemes can
// be learned without first triggering a 401.
type apiAuthentication struct {
	Schemes        []string `json:"schemes"`
	ManagementAPI  string   `json:"management_api"`
	CompatibleAPIs string   `json:"compatible_apis"`
}

func registerAPIDiscoveryRoute(router *gin.RouterGroup, s *Server) {
	router.GET("", func(c *gin.Context) {
		respondSuccess(c, "API discovery", buildAPIDiscovery(s))
	})
}

func buildAPIDiscovery(s *Server) apiDiscoveryResponse {
	return apiDiscoveryResponse{
		APIVersion:          "v1",
		ProviderDiagnostics: describeProviderDiagnostics(s),
		UpdateControl:       "Admin: POST /update/apply with version and nodes (or [all]); workers first, coordinator last, sleeping nodes pending. GET /update/status and /update/history accept ?nodes=all. GET/PATCH /update/settings?node=NAME controls scheduled updates only. Explicit updates remain available when enabled=false. Protocol-breaking changes require an operator maintenance window and rollback of all nodes if any fails.",
		Authentication:      describeAuthentication(s),
		Inference:           describeInference(s),
		EndpointGroups:      collectAPIDiscoveryGroups(s),
		StreamingGETs:       slices.Clone(streamingGETs),
		EnabledFeatures:     collectEnabledFeatures(s),
		Docs: map[string]string{
			"openapi_yaml": "/openapi.yaml",
			"openapi_json": "/openapi.json",
		},
	}
}

func describeProviderDiagnostics(s *Server) string {
	if s.config != nil && s.config.Cluster.IsWorker() {
		return "Query provider diagnostics through the coordinator with ?node=<name>."
	}
	return "GET /providers/{name}/environment?node=<name> observes managed packages, runtime, cached driver information and the selected host toolkit. POST /providers/{name}/install/verify returns prerequisite checks. GET /providers/{name}/service/status?node=<name> reports supervision and the last exit. Admin POST service/start, service/stop and service/restart control managed daemons only; external daemons are read-only. Stopped managed providers return provider_not_running with supervision and the service/start route; unavailable inventories report uncertainty. Host findings contain human remediation; API workarounds use PATCH /providers/{name}/parameters. Host changes are never applied."
}

// describeAuthentication reports the accepted credential schemes and
// what each surface requires. The compat line is read from config
// rather than hardcoded so a node hardened with require_compat_auth
// advertises that instead of inviting anonymous calls that will fail.
func describeAuthentication(s *Server) apiAuthentication {
	return apiAuthentication{
		Schemes: []string{
			"Authorization: Bearer <key>",
			"X-API-Key: <key>",
		},
		ManagementAPI:  "required: admin key for " + apiDiscoveryPrefix + "/*",
		CompatibleAPIs: describeCompatAuth(s),
	}
}

// describeCompatAuth reports what the compatibility surfaces require on
// THIS node. A worker serves them on the cluster mTLS port rather than
// the port this document was fetched from, so telling a caller they are
// anonymous here would point it at an address that does not answer.
func describeCompatAuth(s *Server) string {
	if s.config != nil && s.config.Cluster.IsWorker() {
		return "not served on this port: a worker exposes /v1/* and /api/* on the cluster mTLS port, reachable only through its coordinator"
	}
	if why := anonymousRefusal(s.config, s.access); why != "" {
		return "required: anonymous requests on /v1/* and /api/* are refused because " + why
	}
	return "optional: anonymous requests are accepted on /v1/* and /api/*"
}

// Source of truth = engine.Routes(); the doc cannot lie about what is registered.
func collectAPIDiscoveryGroups(s *Server) map[string][]string {
	groups := map[string][]string{}
	if s.engine == nil {
		return groups
	}
	for _, r := range s.engine.Routes() {
		if !strings.HasPrefix(r.Path, apiDiscoveryPrefix) {
			continue
		}
		// Cluster-to-cluster surface lives on a separate engine in prod; defensive.
		if strings.HasPrefix(r.Path, apiDiscoveryPrefix+"/internal/") {
			continue
		}
		if _, hidden := apiDiscoveryHidden[r.Path]; hidden {
			continue
		}
		if isRetiredRoute(r.Handler) {
			continue
		}
		group := apiDiscoveryGroupFor(r.Path)
		groups[group] = append(groups[group], r.Method+" "+uriTemplatePath(r.Path))
	}
	for k := range groups {
		sort.Strings(groups[k])
	}
	return groups
}

// uriTemplatePath rewrites gin's route syntax into RFC 6570 template
// syntax so the discovery document reads as URI templates rather than
// as this router's internal notation:
//
//	/deployments/:id      -> /deployments/{id}
//	/models/card/*id      -> /models/card/{id}
//
// Both gin forms collapse to the same template because RFC 6570 has no
// notation for "this parameter may contain slashes". That distinction
// is carried by the parameter description in the OpenAPI spec, which is
// where a caller looks for parameter semantics anyway.
func uriTemplatePath(path string) string {
	if !strings.ContainsAny(path, ":*") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		// A bare ":" or "*" is not a parameter, so require a name.
		if len(seg) < 2 {
			continue
		}
		if seg[0] == ':' || seg[0] == '*' {
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// retiredHandlerSuffix identifies the handler every retired route is
// mounted on. Matching the handler rather than listing paths means a
// route retired later drops out of discovery on its own — a hand-kept
// list is exactly the thing that goes stale here.
const retiredHandlerSuffix = ".(*ParamsExecutor).RetiredHandler-fm"

// isRetiredRoute reports whether a route only answers 410 Gone. It is
// the one matcher for that question — discovery, the route catalog and
// the client method guard all ask it here, so a second spelling cannot
// drift away from this one.
//
// Discovery is what an agent enumerates to decide what it can call, so
// listing a route that always refuses hands it a guaranteed dead end.
// /zzrouter/v1/server/routes still reports them, flagged `retired`:
// that one is a literal dump of what is mounted, and the flag is what
// keeps the dump from reading as a menu.
func isRetiredRoute(handler string) bool {
	return strings.HasSuffix(handler, retiredHandlerSuffix)
}

// Hyphens collapse to underscores so group keys keep the underscored
// shape that pre-rewrite consumers (e.g. provider_status, model_groups,
// inference_logs) parse against.
func apiDiscoveryGroupFor(path string) string {
	if path == apiDiscoveryPrefix {
		return "discovery"
	}
	rest := strings.TrimPrefix(path, apiDiscoveryPrefix+"/")
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return "discovery"
	}
	return strings.ReplaceAll(seg, "-", "_")
}

func collectEnabledFeatures(s *Server) []string {
	features := []string{}
	if s.model.Groups != nil {
		features = append(features, "model_groups")
	}
	if s.keyStore != nil && s.access != nil {
		features = append(features, "virtual_keys")
	}
	if s.inference.logStore != nil {
		features = append(features, "inference_logs")
	}
	if s.model.Pricing != nil {
		features = append(features, "pricing")
	}
	if s.updateScheduler != nil {
		features = append(features, "update_control")
		if s.updateScheduler.Enabled() {
			features = append(features, "auto_update")
		}
	}
	return features
}
