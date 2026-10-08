package harness

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// runReadyPollInterval paces the /runs poll in WaitRunHot.
const runReadyPollInterval = 1 * time.Second

// SourceModelID returns the registry-native model id carried by a
// fixture's Source: "huggingface://Qwen/Qwen2.5-1.5B-Instruct-GGUF"
// becomes "Qwen/Qwen2.5-1.5B-Instruct-GGUF". A cloud fixture, or one
// with no scheme, has no registry id and returns "".
func SourceModelID(source string) string {
	if source == "" || source == "cloud" {
		return ""
	}
	if i := strings.Index(source, "://"); i >= 0 {
		return source[i+3:]
	}
	return ""
}

// RegistryModelID is the name to launch a fixture by: the server's
// deploy chain wants the registry-native id and hands it to the resolver
// as-is, so the scheme has to come off first. Falls back to the
// fixture's own ID for models that carry no source.
func RegistryModelID(spec ModelSpec) string {
	if id := SourceModelID(spec.Source); id != "" {
		return id
	}
	return spec.ID
}

// RunModelNames returns every name a launched fixture can appear under
// in /zzrouter/v1/runs.
//
// A launch resolves the caller's name to the one the node knows the
// model by, so a run started as "Qwen/Qwen2.5-1.5B-Instruct-GGUF" is
// listed as "qwen2.5-1.5b-instruct-q4_k_m" — the file it landed in. The
// request name and the row name are both real and neither is derivable
// from the other here, so readiness has to accept either.
func RunModelNames(spec ModelSpec) []string {
	var names []string
	for _, n := range []string{RegistryModelID(spec), spec.ID, fileStem(spec.File)} {
		if n != "" && !contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

func fileStem(file string) string {
	base := path.Base(file)
	if base == "." || base == "/" {
		return ""
	}
	return strings.TrimSuffix(base, path.Ext(base))
}

// WaitRunHot polls /zzrouter/v1/runs until an instance on the target node
// matches the provider and any of modelNames by prefix, reports status
// "running", AND answers /runs/:id/health as healthy.
//
// The two-phase gate matters for llamacpp and vLLM: the run row flips to
// "running" the moment exec returns, but weights are mapped lazily and
// the first /v1/* request can hit a "model_load_failed" 5xx for a few
// seconds. Callers needing stricter readiness should add a probe
// inference on top.
//
// Prefix, not equality: the auto_deploy chain appends a "#variant" hint
// to the model name before launch (e.g. "...#Q4_K_M").
func WaitRunHot(ctx context.Context, c *Client, provider, node string, modelNames []string) error {
	type runRow struct {
		ID        string `json:"id"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Status    string `json:"status"`
		Node      string `json:"node"`
		HealthURL string `json:"health_url"`
	}
	type runsEnvelope struct {
		Data []runRow `json:"data"`
	}

	// lastErr tracks the most recent diagnostic so a ctx-deadline timeout
	// surfaces the underlying cause instead of a bare "context deadline
	// exceeded" — otherwise a five-minute wait ends saying nothing.
	var lastErr error
	for {
		resp, err := c.GET(ctx, "/zzrouter/v1/runs")
		switch {
		case err != nil:
			lastErr = fmt.Errorf("GET /runs: %w", err)
		case resp.Status != 200:
			lastErr = fmt.Errorf("GET /runs: status=%d", resp.Status)
		default:
			var env runsEnvelope
			if jerr := resp.JSON(&env); jerr != nil {
				lastErr = fmt.Errorf("decode /runs: %w", jerr)
			} else {
				lastErr = fmt.Errorf("no matching run yet (provider=%s node=%s model^=%s)",
					provider, node, strings.Join(modelNames, "|"))
				for _, r := range env.Data {
					if r.Provider != provider || r.Node != node || r.Status != "running" ||
						!hasAnyPrefix(r.Model, modelNames) {
						continue
					}
					// Ollama runs declare no health_url — their daemon is
					// one shared process, not one per model — so status
					// running is all there is to check.
					if r.HealthURL == "" || probeRunHealth(ctx, c, r.ID) {
						return nil
					}
					lastErr = fmt.Errorf("run %s matched but unhealthy", r.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("WaitRunHot (last: %v): %w", lastErr, ctx.Err())
			}
			return fmt.Errorf("WaitRunHot: %w", ctx.Err())
		case <-time.After(runReadyPollInterval):
		}
	}
}

// MatchesAnyModelName reports whether a run row's model is any of the
// names the caller knows the model by. Prefix, because the auto_deploy
// chain appends a "#variant" hint before launch.
func MatchesAnyModelName(model string, names []string) bool {
	return hasAnyPrefix(model, names)
}

func hasAnyPrefix(model string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// probeRunHealth asks the cluster's health proxy, which routes through
// coord mesh mTLS to the worker's own /health.
func probeRunHealth(ctx context.Context, c *Client, runID string) bool {
	resp, err := c.GET(ctx, "/zzrouter/v1/runs/"+runID+"/health")
	if err != nil || resp.Status != 200 {
		return false
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if jerr := resp.JSON(&env); jerr != nil {
		return false
	}
	return env.Data.Status == "healthy"
}
