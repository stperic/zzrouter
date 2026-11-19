package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/access/localproof"
)

// ctxKeyLocalOperator marks a request the local-operator gate admitted.
const ctxKeyLocalOperator contextKey = "zzrouter_local_operator"

// localOperatorRefusedHeaders mark a browser or a proxy; a same-host proxy makes every peer look local.
var localOperatorRefusedHeaders = []string{"Origin", "Cookie", "X-Csrf-Token", "Forwarded", "Via", "X-Real-Ip"}

// initLocalProof publishes this process's proof; the open access store means no other process rotates it.
func (s *Server) initLocalProof(ctx context.Context, issuer string) error {
	var err error
	descriptor := localproof.File{TLSCert: s.config.Node.TLSCert}
	if issuer != "" {
		descriptor.ConsoleURL = issuer + "/console/"
	}
	if descriptor.TLSCert != "" {
		descriptor.TLSCert, err = filepath.Abs(descriptor.TLSCert)
		if err != nil {
			return err
		}
	}
	host, err := localProofHost(ctx, s.config.Node.Bind)
	if err != nil {
		return err
	}
	scheme := "http"
	if s.config.Node.IsTLSEnabled() {
		// Without an issuer the console name is the default issuer's.
		scheme, descriptor.TLSName = "https", "localhost"
		if issuer != "" {
			parsed, _ := url.Parse(issuer)
			descriptor.TLSName = parsed.Hostname()
		}
	}
	descriptor.Endpoint = scheme + "://" + net.JoinHostPort(host, strconv.Itoa(s.config.Node.Port))
	s.localProof, err = localproof.New(ctx, s.configDir, descriptor)
	if err != nil {
		return fmt.Errorf("initialize local proof: %w", err)
	}
	return nil
}

// Resolve only daemon configuration, before publishing its proof. Local
// clients then use this literal local address without DNS or environment proxies.
func localProofHost(ctx context.Context, bind string) (string, error) {
	switch bind {
	case "", "0.0.0.0", "localhost":
		return "127.0.0.1", nil
	case "::", "[::]":
		return "::1", nil
	}
	if localproof.Address(bind) {
		return bind, nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, bind)
	if err != nil {
		return "", fmt.Errorf("resolve local proof bind address: %w", err)
	}
	for _, address := range addresses {
		if localproof.Address(address.IP.String()) {
			return address.IP.String(), nil
		}
	}
	return "", fmt.Errorf("the local proof requires the configured bind address to resolve to this host")
}

// localOperatorRequest admits a peer address assigned to this host that sent no browser or proxy header.
func localOperatorRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !localproof.Address(host) {
		return false
	}
	for name := range r.Header {
		name = http.CanonicalHeaderKey(name)
		// A browser sends Sec-Fetch-* even on a same-origin GET without Origin (DNS rebinding).
		if strings.HasPrefix(name, "X-Forwarded-") || strings.HasPrefix(name, "Sec-Fetch-") || slices.Contains(localOperatorRefusedHeaders, name) {
			return false
		}
	}
	return true
}

// localOperator admits an operator on this host and removes the proof so nothing downstream logs or forwards it.
func (s *Server) localOperator(c *gin.Context) {
	proofs := c.Request.Header.Values(localproof.Header)
	c.Request.Header.Del(localproof.Header)
	// One answer for every refusal, so it never says which check failed.
	if len(proofs) != 1 || !localOperatorRequest(c.Request) || !s.localProof.Verify(proofs[0]) {
		Forbidden(c, "Run this command as the installation account on the installed host")
		c.Abort()
		return
	}
	c.Set(string(ctxKeyLocalOperator), true)
	c.Next()
}

// localRoute is the only mount behind localOperator. It records the route, so the catalog
// reports it as local and neither zz_api nor discovery offers it; replay mounts it through
// s.idempotent, which then runs after the gate.
func (s *Server) localRoute(method, route string, replay bool, handler gin.HandlerFunc) {
	group := s.engine.Group("/zzrouter/v1", s.browserCoordinatorOnly, browserResponseHeaders, s.localOperator)
	if replay {
		s.idempotent(group, method, route, nil, handler)
	} else {
		group.Handle(method, route, handler)
	}
	s.localRoutes[method+" "+path.Join(group.BasePath(), route)] = struct{}{}
}

// isLocalOperator reports whether the local-operator gate admitted the request.
func isLocalOperator(c *gin.Context) bool {
	return c != nil && c.GetBool(string(ctxKeyLocalOperator))
}
