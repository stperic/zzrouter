// credential.go — the single place a request's API key is lifted off
// the wire.
//
// Both API surfaces read credentials through here: the management
// surface (/zzrouter/v1/*) and the third-party compatibility surfaces
// (/v1/*, /api/*). Sharing one extractor is what keeps the accepted
// schemes from drifting apart between them, which is the failure an
// agent hits hardest: authenticating successfully against one surface
// and being rejected by the other while holding the same valid key.
//
// Two schemes are accepted everywhere:
//
//	X-API-Key: <key>                (and the role-specific aliases)
//	Authorization: Bearer <key>     (RFC 6750, what LLM SDKs default to)
package server

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
)

const (
	authorizationHeader = "Authorization"
	bearerScheme        = "Bearer"
)

// nodeAPIKeyHeaders is the scheme precedence for coordinator and
// standalone nodes. Authorization sits last so an explicit zzRouter
// header always wins over the SDK-default bearer token when a client
// sends both.
var nodeAPIKeyHeaders = []string{
	"X-API-Key",
	"X-Admin-API-Key",
	"X-User-API-Key",
	"X-Cluster-API-Key",
	authorizationHeader,
}

// callerKeyHeaders are where a caller's own key arrives at an endpoint
// that checks it. The other zzRouter key headers never leave the cluster.
var callerKeyHeaders = []string{"X-API-Key", authorizationHeader}

// upstreamHeaders returns the headers a request forwarded to up carries:
// the caller's, less what describes the caller's connection, less what up
// must not see, plus up's own credential.
func upstreamHeaders(src http.Header, up backend.Upstream) http.Header {
	h := src.Clone()
	wire.StripConnectionHeaders(h)
	scrubForUpstream(h, up)
	return h
}

// scrubForUpstream applies upstreamHeaders' rule to h in place. A reverse
// proxy has already dropped the connection headers, so its Rewrite calls
// this alone.
func scrubForUpstream(h http.Header, up backend.Upstream) {
	// Responses are relayed as they arrive; a compressed stream cannot be
	// flushed chunk by chunk.
	h.Del("Accept-Encoding")
	if up.IsCluster() {
		return
	}
	for _, name := range constants.RoutingHintHeaders() {
		h.Del(name)
	}
	for _, name := range nodeAPIKeyHeaders {
		if up.ForwardsCallerKey() && slices.Contains(callerKeyHeaders, name) {
			continue
		}
		h.Del(name)
	}
	maps.Copy(h, up.Header())
}

// clusterAPIKeyHeaders is the precedence for worker and
// cluster-internal contexts, where the coordinator dispatches with
// X-Cluster-API-Key and that key must win.
var clusterAPIKeyHeaders = []string{
	"X-Cluster-API-Key",
	"X-API-Key",
	authorizationHeader,
}

// compatAPIKeyHeaders is the precedence for the third-party
// compatibility surfaces (/v1/*, /api/*). Authorization leads here, and
// that ordering is load-bearing rather than cosmetic: it is what this
// surface has always resolved first, so a client that sends a bearer
// token AND a stale X-API-Key keeps authenticating on the bearer. The
// two surfaces genuinely disagree about which credential wins, and the
// disagreement is declared here rather than living in two extractors.
var compatAPIKeyHeaders = []string{
	authorizationHeader,
	"X-API-Key",
}

// credential is an API key lifted off a request together with the
// scheme that carried it, so a rejection can name what the caller
// actually sent instead of guessing.
type credential struct {
	key    string
	scheme string
}

// found reports whether a usable key was extracted.
func (cr credential) found() bool { return cr.key != "" }

// extractCredential pulls the API key off a request using the scheme
// precedence appropriate to this node's cluster role. For the
// management surface.
func extractCredential(c *gin.Context, config *pkgConfig.NodeConfig) credential {
	return readCredential(c, acceptedAPIKeyHeaders(c, config))
}

// extractCompatCredential pulls the API key off a request bound for a
// third-party compatibility surface, where Authorization wins.
func extractCompatCredential(c *gin.Context) credential {
	return readCredential(c, compatAPIKeyHeaders)
}

// acceptedAPIKeyHeaders picks the scheme precedence for the request.
func acceptedAPIKeyHeaders(c *gin.Context, config *pkgConfig.NodeConfig) []string {
	if config == nil {
		return nodeAPIKeyHeaders
	}
	if config.Cluster.IsWorker() || clusternode.IsClusterInternal(c.Request.Context()) {
		return clusterAPIKeyHeaders
	}
	return nodeAPIKeyHeaders
}

// readCredential returns the first usable key among the given headers.
// An Authorization header that carries a scheme other than Bearer is
// skipped rather than treated as a raw key, so a Basic or Digest value
// can never be mistaken for a token.
func readCredential(c *gin.Context, headers []string) credential {
	for _, name := range headers {
		raw := strings.TrimSpace(c.GetHeader(name))
		if raw == "" {
			continue
		}
		if name == authorizationHeader {
			token, ok := parseBearer(raw)
			if !ok {
				continue
			}
			return credential{key: token, scheme: authorizationHeader + ": " + bearerScheme}
		}
		return credential{key: raw, scheme: name}
	}
	return credential{}
}

// parseBearer pulls the token out of an RFC 6750 "Bearer <token>"
// value. The scheme token is matched case-insensitively per RFC 7235.
func parseBearer(value string) (string, bool) {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// missingCredentialDetail explains an absent credential in terms of
// what the caller actually sent. A client that used an unsupported
// Authorization scheme is told so by name, because the alternative —
// reporting "API key required" to someone who plainly sent one — is
// the least actionable message the API can return.
func missingCredentialDetail(c *gin.Context) string {
	raw := strings.TrimSpace(c.GetHeader(authorizationHeader))
	if raw == "" {
		return "API key required: send it as \"Authorization: Bearer <key>\" or in the X-API-Key header"
	}
	scheme, _, _ := strings.Cut(raw, " ")
	return fmt.Sprintf(
		"Unsupported Authorization scheme %q: use \"Authorization: Bearer <key>\" or the X-API-Key header",
		scheme,
	)
}
