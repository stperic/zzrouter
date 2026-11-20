package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/calibration"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/pkg/jobs"
)

const (
	// DefaultWaitSeconds reads a snapshot without delaying the agent.
	DefaultWaitSeconds = 0
	// MaxWaitSeconds leaves headroom under Codex's default 60-second timeout.
	MaxWaitSeconds = 45
	// jobPollInterval avoids busy polling while keeping progress responsive.
	jobPollInterval = time.Second
)

// OperationBindings declares the REST operations used by each registered tool.
func OperationBindings() map[string][]string {
	return registerTools(nil, Config{})
}

type overviewInput struct{}
type nodeSummary struct {
	Name             string   `json:"name"`
	Role             string   `json:"role"`
	Health           string   `json:"health"`
	EnabledProviders []string `json:"enabled_providers"`
}
type overviewOutput struct {
	Health         string        `json:"health"`
	Nodes          []nodeSummary `json:"nodes"`
	UnhealthyNodes []nodeHealth  `json:"unhealthy_nodes"`
	EndpointCount  int           `json:"endpoint_count"`
	OpenAPI        string        `json:"openapi"`
}
type nodeHealth struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}
type calibrateInput struct {
	calibration.Request
	retryInput
}

type calibrationOutput struct {
	JobID string `json:"job_id"`
	Node  string `json:"node"`
}

func (out calibrationOutput) summary() string {
	return "Calibration accepted. Read the result with zz_job on the returned node."
}

func (d dispatcher) calibrate(ctx context.Context, in calibrateInput) (calibrationOutput, error) {
	var out struct {
		Data calibrationOutput `json:"data"`
	}
	err := d.call(ctx, request{
		method: http.MethodPost, path: routePrefix("/zzrouter/v1/calibrations").path(),
		jsonBody: in.Request, contentType: "application/json", idempotencyKey: in.IdempotencyKey,
	}, &out)
	return out.Data, err
}

type jobInput struct {
	pageInput
	JobID       string `json:"job_id,omitempty" jsonschema:"Optional job id. Omit to list recent jobs on the coordinator or selected node; supply it to read or wait for one job."`
	Node        string `json:"node,omitempty" jsonschema:"Optional owning node name, never an address."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to poll before returning progress. Default 0, maximum 45. Call again if still running."`
}

func (out overviewOutput) summary() string {
	return fmt.Sprintf("Cluster %s: %d nodes, %d unhealthy nodes. zz_nodes gives a node's OS, GPU, memory and disk.", out.Health, len(out.Nodes), len(out.UnhealthyNodes))
}

func registerTool[In any, Out interface{ summary() string }](s *mcp.Server, cfg Config, bindings map[string][]string, tool *mcp.Tool, ids []string, handler func(dispatcher, context.Context, In) (Out, error)) {
	bindings[tool.Name] = ids
	if s == nil {
		return
	}
	if tool.InputSchema == nil {
		tool.InputSchema = objectSchema[In]{}
	}
	inputSchema, schemaErr := resolveToolSchema(tool.InputSchema)
	outputSchema, outputErr := resolveToolSchema(tool.OutputSchema)
	// SDK typed tools re-marshal arguments through float64 while applying defaults.
	// Keep one raw registration path and let the schema library retain JSON numbers.
	s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := errors.Join(schemaErr, outputErr); err != nil {
			return nil, fmt.Errorf("tool schema: %w", err)
		}
		input, err := applyToolSchema(req.Params.Arguments, inputSchema, true)
		var in In
		if err == nil {
			in, err = decodeToolInput[In](input)
		}
		if err != nil {
			result := &mcp.CallToolResult{}
			result.SetError(fmt.Errorf("validating arguments: %w", err))
			return result, nil
		}
		d, err := newDispatcher(ctx, cfg, bindings)
		var out Out
		if err == nil {
			out, err = handler(d, ctx, in)
		}
		if err != nil {
			result := &mcp.CallToolResult{}
			result.SetError(err)
			return result, nil
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("marshaling output: %w", err)
		}
		encoded, err = applyToolSchema(encoded, outputSchema, false)
		if err != nil {
			return nil, fmt.Errorf("validating tool output: %w", err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.summary()}}, StructuredContent: json.RawMessage(encoded)}, nil
	})
}

func registerTools(s *mcp.Server, cfg Config) map[string][]string {
	bindings := map[string][]string{}
	jobRetentionDescription := fmt.Sprintf("Finished jobs stay readable for %g hours after completion or their latest hand-out, whichever is later; zz_job reports expires_at. Jobs disappear on node restart.", constants.IdempotencyReplayWindow.Hours())
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_overview", OutputSchema: objectSchema[overviewOutput]{}, Description: "Start here: read cluster node names, roles, health, enabled providers and the endpoint count. Use node names as selectors; consult /openapi.yaml for the full REST contract.", Annotations: annotations}, []string{"getApiDiscovery", "listNodes", "getApiHealth"}, dispatcher.overview)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_job", Description: "Omit job_id to list recent jobs on the coordinator or selected node, with limit and offset; this is not a cluster-wide list. Pass job_id here, not run_id, to read state, progress and result or error. A result's run_id belongs to zz_runs, zz_logs, zz_run_restart or zz_run_stop. " + jobRetentionDescription + " Job-not-found hints name an outcome tool. Optionally wait up to 45 seconds; call again when state is pending, running or still running.", Annotations: annotations}, []string{"listJobs", "getJob"}, dispatcher.job)
	registerTool(s, cfg, bindings, &mcp.Tool{
		Name: "zz_calibrate", OutputSchema: objectSchema[calibrationOutput]{},
		Description: "Measure chat or embeddings on running models on one node. Get run_id and target_revision from zz_runs. Supply workloads and request concurrency for each step, or list a resident run without a workload. Concurrency 0 keeps a run idle for that step, so [4,0,4] and [0,8,8] measure each run alone and then both together in one calibration. Change settings with zz_params_set and restart_affected, or zz_run_start and replace. Read the terminal report with zz_job using job_id and node. " + jobRetentionDescription,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool)},
	}, []string{"createCalibration"}, dispatcher.calibrate)
	registerAgentTools(s, cfg, bindings, jobRetentionDescription)
	registerAPITools(s, cfg, bindings)
	registerProviderTools(s, cfg, bindings, false)
	return bindings
}

func (d dispatcher) overview(ctx context.Context, _ overviewInput) (overviewOutput, error) {
	var discovery struct {
		Data struct {
			EndpointGroups map[string][]string `json:"endpoint_groups"`
		} `json:"data"`
	}
	var nodes struct {
		Data []struct {
			Name      string `json:"name"`
			Role      string `json:"cluster_role"`
			Health    string `json:"health_status"`
			Providers []struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"providers"`
		} `json:"data"`
	}
	var health struct {
		Data struct {
			Status string `json:"cluster_health"`
			Nodes  []struct {
				Name   string `json:"hostname"`
				Status string `json:"status"`
			} `json:"nodes"`
		} `json:"data"`
	}
	for _, call := range []struct {
		path routePrefix
		out  any
	}{{discoveryRoute, &discovery}, {nodesRoute, &nodes}, {healthRoute, &health}} {
		if err := d.call(ctx, request{method: http.MethodGet, path: call.path.path()}, call.out); err != nil {
			return overviewOutput{}, err
		}
	}
	out := overviewOutput{Health: health.Data.Status, Nodes: []nodeSummary{}, UnhealthyNodes: []nodeHealth{}, OpenAPI: "/openapi.yaml"}
	for _, n := range nodes.Data {
		summary := nodeSummary{Name: n.Name, Role: n.Role, Health: n.Health, EnabledProviders: []string{}}
		for _, p := range n.Providers {
			name := p.Key
			if name == "" {
				name = p.Name
			}
			summary.EnabledProviders = append(summary.EnabledProviders, name)
		}
		sort.Strings(summary.EnabledProviders)
		out.Nodes = append(out.Nodes, summary)
	}
	for _, n := range health.Data.Nodes {
		if n.Status != "healthy" {
			out.UnhealthyNodes = append(out.UnhealthyNodes, nodeHealth{Name: n.Name, Status: n.Status})
		}
	}
	for _, endpoints := range discovery.Data.EndpointGroups {
		out.EndpointCount += len(endpoints)
	}
	return out, nil
}

func (d dispatcher) job(ctx context.Context, input jobInput) (out agentOutput, err error) {
	if input.JobID == "" {
		if input.WaitSeconds != 0 {
			return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "wait_seconds requires job_id", nil)
		}
		return d.list(ctx, routePrefix(strings.TrimSuffix(string(jobsRoute), "/")).path(), input.pageInput, url.Values{"node": {input.Node}}, "", "job_id node kind phase:state percent:progress step err:error expires_at", "jobs")
	}
	defer func() {
		if err == nil {
			out.text = fmt.Sprintf("Job %s: %s, %v%%.", out.fields["job_id"], out.fields["state"], out.fields["progress"])
		}
	}()
	path, err := pathSegment(jobsRoute, input.JobID)
	if err != nil {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, "job_id: "+err.Error(), nil)
	}
	if input.WaitSeconds < DefaultWaitSeconds || input.WaitSeconds > MaxWaitSeconds {
		return agentOutput{}, newToolError(http.StatusBadRequest, codeInvalidInput, fmt.Sprintf("wait_seconds must be between %d and %d", DefaultWaitSeconds, MaxWaitSeconds), nil)
	}
	// Bound in-flight REST calls, not just the delay between polls.
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if input.WaitSeconds > DefaultWaitSeconds {
		var end context.CancelFunc
		pollCtx, end = context.WithTimeout(pollCtx, time.Duration(input.WaitSeconds)*time.Second)
		defer end()
	}
	stop := context.AfterFunc(d.outer.ctx, cancel) //nolint:contextcheck // Decision 2 bridges outer cancellation without copying auth values.
	defer stop()
	out = agentOutput{fields: map[string]any{"job_id": input.JobID, "state": "still running, call zz_job again", "progress": 0}}
	for {
		var response struct {
			Data jobs.Event `json:"data"`
		}
		if err := d.call(pollCtx, request{method: http.MethodGet, path: path, query: url.Values{"node": {input.Node}}}, &response); err != nil {
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				out.fields["job_id"], out.fields["state"] = input.JobID, "still running, call zz_job again"
				return out, nil
			}
			return agentOutput{}, err
		}
		ev := response.Data
		out.fields = map[string]any{"job_id": ev.JobID, "state": string(ev.Phase), "progress": ev.Percent, "step": ev.Step, "error": ev.Err}
		if ev.Phase.IsTerminal() {
			out.fields["expires_at"] = ev.ExpiresAt
			out.fields["result"] = ev.Meta
			delete(ev.Meta, "health_url")
			if id, ok := ev.Meta["instance_id"]; ok {
				ev.Meta["run_id"] = id
				delete(ev.Meta, "instance_id")
			}
			return out, nil
		}
		if input.WaitSeconds == DefaultWaitSeconds {
			return out, nil
		}
		// No time left for another poll: answer now rather than race the deadline.
		if deadline, ok := pollCtx.Deadline(); ok && time.Until(deadline) <= jobPollInterval {
			out.fields["job_id"], out.fields["state"] = input.JobID, "still running, call zz_job again"
			return out, nil
		}
		timer := time.NewTimer(jobPollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				out.fields["job_id"], out.fields["state"] = input.JobID, "still running, call zz_job again"
				return out, nil
			}
			return agentOutput{}, newToolError(httperr.StatusClientClosedRequest, codeInterrupted, fmt.Sprintf("job wait interrupted: %v", pollCtx.Err()), pollCtx.Err())
		case <-timer.C:
		}
	}
}
