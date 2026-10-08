package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

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
	Health           string        `json:"health"`
	Nodes            []nodeSummary `json:"nodes"`
	EnabledProviders []string      `json:"enabled_providers"`
	EndpointCount    int           `json:"endpoint_count"`
	OpenAPI          string        `json:"openapi"`
}
type jobInput struct {
	JobID       string `json:"job_id" jsonschema:"Job id returned by an operation. Follow it until state is done or failed."`
	Node        string `json:"node,omitempty" jsonschema:"Optional owning node name, never an address."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to poll before returning progress. Default 0, maximum 45. Call again if still running."`
}
type jobOutput struct {
	JobID    string     `json:"job_id"`
	State    jobs.Phase `json:"state"`
	Progress int        `json:"progress"`
	Step     string     `json:"step,omitempty"`
	Result   jobs.Meta  `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
}

func (out overviewOutput) summary() string {
	return fmt.Sprintf("Cluster %s: %d nodes, %d enabled providers.", out.Health, len(out.Nodes), len(out.EnabledProviders))
}
func (out jobOutput) summary() string {
	return fmt.Sprintf("Job %s: %s, %d%%.", out.JobID, out.State, out.Progress)
}

func registerTool[In any, Out interface{ summary() string }](s *mcp.Server, cfg Config, bindings map[string][]string, tool *mcp.Tool, ids []string, handler func(dispatcher, context.Context, In) (Out, error)) {
	bindings[tool.Name] = ids
	if s == nil {
		return
	}
	tool.InputSchema = objectSchema[In]{}
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		var out Out
		d, err := newDispatcher(ctx, cfg)
		if err == nil {
			out, err = handler(d, ctx, in)
		}
		if err != nil {
			return nil, out, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.summary()}}}, out, nil
	})
}

func registerTools(s *mcp.Server, cfg Config) map[string][]string {
	bindings := map[string][]string{}
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(bool)}
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_overview", OutputSchema: objectSchema[overviewOutput]{}, Description: "Start here: read cluster node names, roles, health, enabled providers and the endpoint count. Use node names as selectors; consult /openapi.yaml for the full REST contract.", Annotations: annotations}, []string{"getApiDiscovery", "listNodes", "getApiHealth"}, dispatcher.overview)
	registerTool(s, cfg, bindings, &mcp.Tool{Name: "zz_job", Description: "Read a job's state, progress and result or error. Optionally wait up to 45 seconds; call again when state is pending, running or still running.", Annotations: annotations}, []string{"getJob"}, dispatcher.job)
	registerAgentTools(s, cfg, bindings)
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
	out := overviewOutput{Health: health.Data.Status, Nodes: []nodeSummary{}, EnabledProviders: []string{}, OpenAPI: "/openapi.yaml"}
	providers := map[string]bool{}
	for _, n := range nodes.Data {
		summary := nodeSummary{Name: n.Name, Role: n.Role, Health: n.Health, EnabledProviders: []string{}}
		for _, p := range n.Providers {
			name := p.Key
			if name == "" {
				name = p.Name
			}
			summary.EnabledProviders = append(summary.EnabledProviders, name)
			providers[name] = true
		}
		sort.Strings(summary.EnabledProviders)
		out.Nodes = append(out.Nodes, summary)
	}
	for name := range providers {
		out.EnabledProviders = append(out.EnabledProviders, name)
	}
	for _, endpoints := range discovery.Data.EndpointGroups {
		out.EndpointCount += len(endpoints)
	}
	sort.Strings(out.EnabledProviders)
	return out, nil
}

func (d dispatcher) job(ctx context.Context, input jobInput) (jobOutput, error) {
	path, err := pathSegment(jobsRoute, input.JobID)
	if err != nil {
		return jobOutput{}, fmt.Errorf("job_id: %w", err)
	}
	if input.WaitSeconds < DefaultWaitSeconds || input.WaitSeconds > MaxWaitSeconds {
		return jobOutput{}, fmt.Errorf("wait_seconds must be between %d and %d", DefaultWaitSeconds, MaxWaitSeconds)
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
	out := jobOutput{JobID: input.JobID, State: "still running, call zz_job again"}
	for {
		var response struct {
			Data jobs.Event `json:"data"`
		}
		if err := d.call(pollCtx, request{method: http.MethodGet, path: path, query: url.Values{"node": {input.Node}}}, &response); err != nil {
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				out.JobID, out.State = input.JobID, "still running, call zz_job again"
				return out, nil
			}
			return jobOutput{}, err
		}
		ev := response.Data
		out = jobOutput{JobID: ev.JobID, State: ev.Phase, Progress: ev.Percent, Step: ev.Step, Error: ev.Err}
		if ev.Phase.IsTerminal() {
			out.Result = ev.Meta
			return out, nil
		}
		if input.WaitSeconds == DefaultWaitSeconds {
			return out, nil
		}
		timer := time.NewTimer(jobPollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				out.JobID, out.State = input.JobID, "still running, call zz_job again"
				return out, nil
			}
			return jobOutput{}, fmt.Errorf("job wait interrupted: %w", pollCtx.Err())
		case <-timer.C:
		}
	}
}
