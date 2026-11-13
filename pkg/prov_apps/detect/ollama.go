package detect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stperic/common/httpclient"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
)

// ollamaProbeTimeout bounds the boot-time Ollama port ping. Ollama's
// /api/version endpoint responds in low-single-digit milliseconds on a
// healthy daemon; 2s is generous enough to survive a momentarily busy
// local process without blocking startup.
const ollamaProbeTimeout = 2 * time.Second

// ProbeOllama queries an Ollama daemon's /api/version endpoint and returns
// the reported version string. The second return value is true iff the
// daemon responded with a parsable version.
//
// This is zzRouter's only non-managed provider probe. Ollama is external-
// by-design (a separate daemon the user runs independently) and has a
// stable version endpoint, so a single HTTP GET replaces the generic
// detection rules engine for this one provider.
//
// It GETs OllamaVersionEndpoint(svc) carrying svc's declared credential.
func ProbeOllama(ctx context.Context, svc *config.ServiceConfig, resolvers ...*backend.Resolver) (string, bool) {
	endpoint := OllamaVersionEndpoint(svc)
	if endpoint == "" {
		return "", false
	}
	probeCtx, cancel := context.WithTimeout(ctx, ollamaProbeTimeout)
	defer cancel()

	target, ok := backend.Target(svc)
	if len(resolvers) > 0 {
		target, ok = resolvers[0].Target("ollama")
	}
	if !ok {
		return "", false
	}
	// Only the declared path travels to the coherently resolved daemon.
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	base, err := url.Parse(target.Endpoint)
	if err != nil {
		return "", false
	}
	probePath := parsed.RequestURI()
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	if prefix != "" && (parsed.EscapedPath() == prefix || strings.HasPrefix(parsed.EscapedPath(), prefix+"/")) {
		probePath = strings.TrimPrefix(probePath, prefix)
	}
	req, err := target.NewRequest(probeCtx, http.MethodGet, probePath, nil)
	if err != nil {
		return "", false
	}
	client := httpclient.SameOrigin(&http.Client{Transport: &http.Transport{DisableKeepAlives: true}})
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false
	}
	if body.Version == "" {
		return "", false
	}
	return body.Version, true
}

// OllamaVersionEndpoint returns the version URL declared in the Ollama
// provider's YAML discovery rules, or "" if none is configured. Config
// stays the single source of truth — ProbeOllama never hardcodes a URL.
func OllamaVersionEndpoint(svc *config.ServiceConfig) string {
	if svc == nil || svc.Discovery == nil {
		return ""
	}
	for _, rule := range svc.Discovery.Detection {
		if strings.EqualFold(rule.Method, "http") && rule.Target != "" {
			return rule.Target
		}
	}
	return ""
}
