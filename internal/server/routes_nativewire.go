// Native-wire passthrough routes. Each configured mount exposes a
// vendor-native API surface (Anthropic Messages, Vertex AI, Bedrock,
// etc.) on a dedicated prefix so vendor SDKs can point at zzrouter
// by swapping only their base URL. The set of mounts is driven
// entirely by node_yaml native_wire.mounts; nothing is hardcoded here.
package server

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// registerNativeWireRoutes wires every configured mount onto the
// engine. Rejections use the OpenAI error envelope because it is the
// closest common shape across vendor SDKs; successful responses pass
// through unchanged.
func (s *Server) registerNativeWireRoutes() int {
	mounts := s.config.NativeWire.Mounts
	if len(mounts) == 0 {
		return 0
	}

	openaiResponder := s.responders.openai
	count := 0
	for mount, providerKey := range mounts {
		mount = strings.Trim(mount, "/")
		if mount == "" || providerKey == "" {
			continue
		}

		group := s.engine.Group("/"+mount,
			httperr.AttachResponder(openaiResponder),
			s.auth.OptionalAuthMiddleware(),
		)
		boundMount := mount
		boundProvider := providerKey
		group.Any("/*path", func(c *gin.Context) {
			s.handleNativeWirePassthrough(c, boundMount, boundProvider)
		})
		count++
	}

	utils.LogDebugf("[Routes] Registered native-wire passthrough mounts: %d", count)
	return count
}

func (s *Server) handleNativeWirePassthrough(c *gin.Context, mount, providerKey string) {
	resolved, ok := s.backend.Resolve(providerKey)
	if !ok {
		s.responders.openai.Unavailable(c,
			"Configured backend for native-wire mount /"+mount+" is not available. Check that the provider is enabled and exposes a running endpoint.")
		return
	}

	if !s.admitOpaqueForward(c, "/"+mount, resolved.Cloud) {
		return
	}
	release, ok := s.enforceAndAttribute(c, nativeWireLabel(mount), nil)
	if !ok {
		return
	}
	defer release()

	// readPassthroughBody also enforces the configured upload cap so
	// multipart vendor uploads (Anthropic/Vertex file APIs) are
	// preserved for replay by proxyToBackend.
	body, bodyOK := s.readPassthroughBody(c)
	if !bodyOK {
		return
	}

	// Gin's *path wildcard includes the leading slash. An empty suffix
	// (e.g. GET /anthropic) becomes "/" so vendors that treat root as
	// a probe still receive the call.
	suffix := c.Param("path")
	if suffix == "" {
		suffix = "/"
	}
	target := backend.PassthroughTarget(resolved.Endpoint, suffix, c.Request.URL.RawQuery)
	s.proxy.ForwardToBackend(c.Writer, c.Request, providerKey, target, resolved.Upstream, body)
}

// nativeWireLabel names a mount where a model name would go: in rate-limit
// accounting and in any refusal.
func nativeWireLabel(mount string) string {
	return "native-wire /" + mount
}

// admitOpaqueForward refuses the callers a surface that forwards requests
// it does not read (a native-wire or MCP mount) cannot hold to their
// limits. It knows neither the model nor the price, so a team model
// allow-list, a spend limit and a tokens-per-minute limit would all pass
// unenforced. Request-rate and concurrency limits need neither, and
// Enforce applies them. billed marks a surface that forwards with this
// node's own upstream credential, which a keyless caller would spend.
func (s *Server) admitOpaqueForward(c *gin.Context, surface string, billed bool) bool {
	ac := GetAccessContext(c)
	if ac == nil {
		if billed && !s.access.AnonymousCloudAllowed() {
			s.responders.openai.Unauthorized(c, surface+" forwards with this node's own upstream credential; "+
				"authenticate with an API key")
			return false
		}
		return true
	}
	if ac.Key == nil || !ac.Key.IsVirtual {
		return true
	}
	var unenforceable string
	switch {
	case ac.Team != nil && len(ac.Team.AllowedModels) > 0:
		unenforceable = "its team limits which models it may use"
	case ac.Key.Quotas.SpendLimit > 0 || (ac.Team != nil && ac.Team.Quotas.SpendLimit > 0):
		unenforceable = "it has a spend limit"
	case ac.Key.Quotas.TPMLimit > 0 || (ac.Team != nil && ac.Team.Quotas.TPMLimit > 0):
		unenforceable = "it has a tokens-per-minute limit"
	default:
		return true
	}
	s.responders.openai.Forbidden(c, fmt.Sprintf("key %q cannot use %s: %s, and %s forwards requests "+
		"without reading them, so it cannot tell which model one uses or what it costs", ac.Key.ID, surface, unenforceable, surface))
	return false
}
