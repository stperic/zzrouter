package server

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web/console/*.html web/console/*.css web/console/*.mjs web/console/icons/*.svg web/console/vendor/*.mjs web/console/vendor/*.js web/console/vendor/*.txt
var consoleAssets embed.FS

// registerBrowserConsole exposes a fixed public shell; the existing API owns
// every identity, permission, provider operation and protected value.
func (s *Server) registerBrowserConsole() {
	if s.access.oauth == nil {
		return
	}
	console := s.engine.Group("/console", browserResponseHeaders, func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; worker-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		c.Next()
	}, s.browserCoordinatorOnly)
	console.GET("", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/console/") })
	for _, asset := range []struct{ path, file, contentType string }{
		{"/", "index.html", "text/html; charset=utf-8"},
		{"/console.css", "console.css", "text/css; charset=utf-8"},
		{"/console.mjs", "console.mjs", "text/javascript; charset=utf-8"},
		{"/api.mjs", "api.mjs", "text/javascript; charset=utf-8"},
		{"/chat.mjs", "chat.mjs", "text/javascript; charset=utf-8"},
		{"/vendor/chat.mjs", "vendor/chat.mjs", "text/javascript; charset=utf-8"},
		{"/vendor/chat-transport.mjs", "vendor/chat-transport.mjs", "text/javascript; charset=utf-8"},
		{"/observations.mjs", "observations.mjs", "text/javascript; charset=utf-8"},
		{"/live-reads.mjs", "live-reads.mjs", "text/javascript; charset=utf-8"},
		{"/diagnostics.mjs", "diagnostics.mjs", "text/javascript; charset=utf-8"},
		{"/workloads.mjs", "workloads.mjs", "text/javascript; charset=utf-8"},
		{"/workload-data.mjs", "workload-data.mjs", "text/javascript; charset=utf-8"},
		{"/workload-details.mjs", "workload-details.mjs", "text/javascript; charset=utf-8"},
		{"/run-form.mjs", "run-form.mjs", "text/javascript; charset=utf-8"},
		{"/dom.mjs", "dom.mjs", "text/javascript; charset=utf-8"},
		{"/providers.mjs", "providers.mjs", "text/javascript; charset=utf-8"},
		{"/inventory.mjs", "inventory.mjs", "text/javascript; charset=utf-8"},
		{"/network-data.mjs", "network-data.mjs", "text/javascript; charset=utf-8"},
		{"/details.mjs", "details.mjs", "text/javascript; charset=utf-8"},
		{"/nodes.mjs", "nodes.mjs", "text/javascript; charset=utf-8"},
		{"/clients.mjs", "clients.mjs", "text/javascript; charset=utf-8"},
		{"/overview.mjs", "overview.mjs", "text/javascript; charset=utf-8"},
		{"/topology.mjs", "topology.mjs", "text/javascript; charset=utf-8"},
		{"/topology-data.mjs", "topology-data.mjs", "text/javascript; charset=utf-8"},
		{"/provider-icons.mjs", "provider-icons.mjs", "text/javascript; charset=utf-8"},
		{"/vendor/graph.mjs", "vendor/graph.mjs", "text/javascript; charset=utf-8"},
		{"/vendor/elk-worker.min.js", "vendor/elk-worker.min.js", "text/javascript; charset=utf-8"},
		{"/vendor/THIRD_PARTY_NOTICES.txt", "vendor/THIRD_PARTY_NOTICES.txt", "text/plain; charset=utf-8"},
		{"/icons/client.svg", "icons/client.svg", "image/svg+xml"},
		{"/icons/coordinator.svg", "icons/coordinator.svg", "image/svg+xml"},
		{"/icons/node.svg", "icons/node.svg", "image/svg+xml"},
		{"/icons/cloud.svg", "icons/cloud.svg", "image/svg+xml"},
		{"/icons/provider.svg", "icons/provider.svg", "image/svg+xml"},
		{"/icons/provider-openai.svg", "icons/provider-openai.svg", "image/svg+xml"},
		{"/icons/provider-anthropic.svg", "icons/provider-anthropic.svg", "image/svg+xml"},
		{"/icons/provider-azure.svg", "icons/provider-azure.svg", "image/svg+xml"},
		{"/icons/provider-bedrock.svg", "icons/provider-bedrock.svg", "image/svg+xml"},
		{"/icons/provider-cloudflare.svg", "icons/provider-cloudflare.svg", "image/svg+xml"},
		{"/icons/provider-gemini.svg", "icons/provider-gemini.svg", "image/svg+xml"},
		{"/icons/provider-groq.svg", "icons/provider-groq.svg", "image/svg+xml"},
		{"/icons/provider-openrouter.svg", "icons/provider-openrouter.svg", "image/svg+xml"},
		{"/icons/provider-ollama.svg", "icons/provider-ollama.svg", "image/svg+xml"},
		{"/icons/provider-comfyui.svg", "icons/provider-comfyui.svg", "image/svg+xml"},
		{"/icons/provider-vllm.svg", "icons/provider-vllm.svg", "image/svg+xml"},
		{"/icons/provider-huggingface.svg", "icons/provider-huggingface.svg", "image/svg+xml"},
		{"/icons/provider-mlx.svg", "icons/provider-mlx.svg", "image/svg+xml"},
		{"/icons/provider-llamacpp.svg", "icons/provider-llamacpp.svg", "image/svg+xml"},
		{"/icons/provider-strata.svg", "icons/provider-strata.svg", "image/svg+xml"},
		{"/icons/status-healthy.svg", "icons/status-healthy.svg", "image/svg+xml"},
		{"/icons/status-down.svg", "icons/status-down.svg", "image/svg+xml"},
		{"/icons/status-degraded.svg", "icons/status-degraded.svg", "image/svg+xml"},
		{"/icons/status-unknown.svg", "icons/status-unknown.svg", "image/svg+xml"},
		{"/access.mjs", "access.mjs", "text/javascript; charset=utf-8"},
		{"/routes.mjs", "routes.mjs", "text/javascript; charset=utf-8"},
		{"/route-form.mjs", "route-form.mjs", "text/javascript; charset=utf-8"},
		{"/access-form.mjs", "access-form.mjs", "text/javascript; charset=utf-8"},
		{"/access-policy.mjs", "access-policy.mjs", "text/javascript; charset=utf-8"},
		{"/access-budget.mjs", "access-budget.mjs", "text/javascript; charset=utf-8"},
	} {
		body, err := consoleAssets.ReadFile("web/console/" + asset.file)
		if err != nil {
			panic("missing embedded console asset: " + asset.file)
		}
		console.GET(asset.path, func(c *gin.Context) { c.Data(http.StatusOK, asset.contentType, body) })
	}
}
