package server

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type providerBrowserTarget struct {
	node, run, prefix string
	suffix            *url.URL
}

func parseProviderBrowserTarget(u *url.URL) (providerBrowserTarget, bool) {
	parts := strings.SplitN(u.EscapedPath(), "/", 5)
	if len(parts) != 5 || parts[0] != "" || parts[1] != "runs" {
		return providerBrowserTarget{}, false
	}
	node, nodeOK := providerBrowserSegment(parts[2])
	run, runOK := providerBrowserSegment(parts[3])
	if !nodeOK || !runOK {
		return providerBrowserTarget{}, false
	}
	suffix, err := url.ParseRequestURI("/" + parts[4])
	if err != nil {
		return providerBrowserTarget{}, false
	}
	suffix.RawQuery = u.RawQuery
	return providerBrowserTarget{node: node, run: run, prefix: "/runs/" + parts[2] + "/" + parts[3], suffix: suffix}, true
}

func providerBrowserSegment(raw string) (string, bool) {
	value, err := url.PathUnescape(raw)
	return value, err == nil && value != "" && len(value) <= 255 && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n") && url.PathEscape(value) == raw
}

func providerBrowserRequestAllowed(r *http.Request, origin *url.URL) bool {
	if r.Host != origin.Host || len(r.Header.Values("Origin")) > 1 {
		return false
	}
	supplied := r.Header.Get("Origin")
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead
	if supplied != "" && supplied != origin.String() || (unsafe || upgrade) && supplied != origin.String() {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "" || site == "none" || site == "same-origin" {
		return true
	}
	return !unsafe && !upgrade && r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}

// Browser paths require the published node name, never an address or mesh alias.
func (s *Server) providerBrowserNodeKnown(node string) bool {
	if node != "" && node == s.node.Name() {
		return true
	}
	if s.cluster.coordinator != nil {
		for _, endpoint := range s.cluster.coordinator.GetAllEndpoints() {
			if !endpoint.IsLocal && endpoint.NodeName != "" && endpoint.NodeName == node {
				return true
			}
		}
	}
	return false
}

func (s *Server) forwardProviderBrowser(w http.ResponseWriter, r *http.Request, destination providerBrowserTarget) {
	if !s.providerBrowserNodeKnown(destination.node) {
		http.NotFound(w, r)
		return
	}
	if destination.node == s.node.Name() {
		s.serveProviderRunBrowser(w, r, destination.run, destination.suffix, &destination)
		return
	}
	endpoint := s.resolveNodeToClusterURL(destination.node)
	target, err := url.Parse(endpoint)
	client := s.coordWorkerMTLSClient()
	if endpoint == "" || err != nil || target.Scheme != "https" || target.User != nil || client == nil {
		http.Error(w, "Provider node unavailable.", http.StatusServiceUnavailable)
		return
	}
	path := "/zzrouter/v1/internal/runs/" + url.PathEscape(destination.run) + "/browser" + destination.suffix.EscapedPath()
	proxy := &httputil.ReverseProxy{
		Transport:     client.Transport,
		FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme, p.Out.URL.Host = target.Scheme, target.Host
			p.Out.URL.Path, _ = url.PathUnescape(path)
			p.Out.URL.RawPath, p.Out.URL.RawQuery = path, destination.suffix.RawQuery
			p.Out.Host = p.In.Host
			scrubProviderBrowserRequest(p.Out.Context(), p.Out.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			scrubProviderBrowserResponse(response.Header)
			return rewriteProviderBrowserRedirect(response, &destination)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() == nil {
				http.Error(w, "Provider node unavailable.", http.StatusBadGateway)
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

func rewriteProviderBrowserRedirect(response *http.Response, destination *providerBrowserTarget) error {
	if destination == nil || response.Header.Get("Location") == "" {
		return nil
	}
	next, err := providerBrowserRedirect(response.Header.Get("Location"), *destination)
	if err != nil {
		return err
	}
	response.Header.Set("Location", next)
	return nil
}

func providerBrowserRedirect(raw string, destination providerBrowserTarget) (string, error) {
	next, err := url.Parse(raw)
	if err != nil || next.IsAbs() || next.Host != "" || next.User != nil || next.Opaque != "" || strings.Contains(next.Path, "\\") {
		return "", errors.New("provider redirect leaves its run")
	}
	next = destination.suffix.ResolveReference(next)
	for _, part := range strings.Split(next.Path, "/") {
		if part == "." || part == ".." {
			return "", errors.New("invalid provider redirect path")
		}
	}
	return destination.prefix + next.String(), nil
}
