package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

type providerDiscoveryInput struct {
	Provider string `json:"provider,omitempty" jsonschema:"Optional exact provider name."`
	Node     string `json:"node,omitempty" jsonschema:"Optional exact node name, never an address."`
	RunID    string `json:"run_id,omitempty" jsonschema:"Optional exact run identity."`
	Model    string `json:"model,omitempty" jsonschema:"Optional exact model identity."`
	Runtime  string `json:"runtime,omitempty" jsonschema:"Optional exact runtime identity."`
	Limit    *int   `json:"limit,omitempty" jsonschema:"Maximum entries, from 1 to 100. Default 50."`
	Offset   *int   `json:"offset,omitempty" jsonschema:"Nonnegative entries to skip. Default 0."`
}

func (in providerDiscoveryInput) query() url.Values {
	query := url.Values{"provider": {in.Provider}, "node": {in.Node}, "run_id": {in.RunID}, "model": {in.Model}, "runtime": {in.Runtime}}
	if in.Limit != nil {
		query.Set("limit", strconv.Itoa(*in.Limit))
	}
	if in.Offset != nil {
		query.Set("offset", strconv.Itoa(*in.Offset))
	}
	return query
}

type operationDescriptionInput struct {
	OperationID string `json:"operation_id" jsonschema:"Provider-qualified operation ID returned by operation discovery."`
}

type operationExecuteInput struct {
	OperationID        string                           `json:"operation_id" jsonschema:"Provider-qualified operation ID returned by operation discovery."`
	Version            int                              `json:"version" jsonschema:"Contract version from the current operation description."`
	DefinitionRevision string                           `json:"definition_revision" jsonschema:"Exact definition revision from the current operation description."`
	Target             prov_apps.OperationTarget        `json:"target" jsonschema:"Exact provider, node, run, model and runtime identity from discovery."`
	Preconditions      prov_apps.OperationPreconditions `json:"preconditions" jsonschema:"Exact target revision from current discovery."`
	Input              json.RawMessage                  `json:"input" jsonschema:"Native JSON input. The selected operation schema is validated by REST before execution."`
}

// registerProviderTools binds both audiences to the same REST discovery and execution contracts.
func registerProviderTools(s *mcp.Server, cfg Config, bindings map[string][]string, user bool) {
	name, api, ids := "zz_", "/zzrouter/v1", []string{"listOperations", "describeOperation", "executeOperation", "listConnections"}
	if user {
		name, api, ids = "zzrouter_", "/inference/v1", []string{"listInferenceOperations", "describeInferenceOperation", "executeInferenceOperation", "listInferenceConnections"}
	}
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: name + "operations", Description: "Discover visible native operation summaries and exact target revisions. Fetch one operation's description for schemas before execution. Definition evidence does not prove runtime capability.", Annotations: read}, ids[:1],
		func(d dispatcher, ctx context.Context, in providerDiscoveryInput) (jsonOutput, error) {
			return jsonCall(d, ctx, request{method: http.MethodGet, path: requestPath{api + "/operations"}, query: in.query()})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: name + "operation", Description: "Describe one visible native operation, including its input/output JSON schemas, definition revision and exact target preconditions.", Annotations: read}, ids[1:2],
		func(d dispatcher, ctx context.Context, in operationDescriptionInput) (jsonOutput, error) {
			path, err := pathSegment(routePrefix(api+"/operations/"), in.OperationID)
			if err != nil {
				return nil, err
			}
			return jsonCall(d, ctx, request{method: http.MethodGet, path: path})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: name + "operation_execute", Description: "Execute one discovered operation against its exact target and revisions. Nested input is validated by REST. JSON only, non-streaming, with 1 MiB request/result limits; binary and realtime transports are unsupported. The current diagnostic adapter requires an administrator.", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)}}, ids[2:3],
		func(d dispatcher, ctx context.Context, in operationExecuteInput) (jsonOutput, error) {
			path, err := childPath(routePrefix(api+"/operations/"), in.OperationID, "/execute")
			if err != nil {
				return nil, err
			}
			return jsonCall(d, ctx, request{method: http.MethodPost, path: path, jsonBody: in, omit: []string{"operation_id"}, contentType: "application/json"})
		})
	registerTool(s, cfg, bindings, &mcp.Tool{Name: name + "connections", Description: "Read permitted native, routed and optional browser connection metadata for any purpose. No runs start and no networking changes. Native authentication is separate; remote reachability remains unknown. Credentials are omitted.", Annotations: read}, ids[3:],
		func(d dispatcher, ctx context.Context, in providerDiscoveryInput) (jsonOutput, error) {
			return jsonCall(d, ctx, request{method: http.MethodGet, path: requestPath{api + "/connections"}, query: in.query()})
		})
}
