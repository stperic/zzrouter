package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func (s *Server) handleInternalProviderBrowser(c *gin.Context) {
	prefix := "/zzrouter/v1/internal/runs/" + url.PathEscape(c.Param("id")) + "/browser"
	raw := c.Request.URL.EscapedPath()
	if !strings.HasPrefix(raw, prefix+"/") {
		http.NotFound(c.Writer, c.Request)
		return
	}
	suffix, err := url.ParseRequestURI(strings.TrimPrefix(raw, prefix))
	if err != nil {
		http.NotFound(c.Writer, c.Request)
		return
	}
	suffix.RawQuery = c.Request.URL.RawQuery
	s.serveProviderRunBrowser(c.Writer, c.Request, c.Param("id"), suffix, nil)
}

func (s *Server) serveProviderRunBrowser(w http.ResponseWriter, r *http.Request, id string, suffix *url.URL, public *providerBrowserTarget) {
	run, endpoint, err := s.providers.appMgr.BrowserRun(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.providers.browserAdmitted != nil {
		s.providers.browserAdmitted()
	}
	target, parseErr := url.Parse(endpoint)
	if parseErr != nil || target.Scheme != "http" || target.User != nil || !isLoopbackBrowserTarget(target) {
		http.NotFound(w, r)
		return
	}
	forward := r.Clone(r.Context())
	path := *suffix
	forward.URL = &path
	limit := int64(defaultMaxRequestSize)
	if s.config != nil && s.config.Node.MaxRequestSize > 0 {
		limit = s.config.Node.MaxRequestSize
	}
	//nolint:contextcheck // The handler merges this request's context with the run lifetime before dialing.
	newProviderBrowserProxy(run, target, s.proxy.streamClient.Transport, limit, public).ServeHTTP(w, forward)
}

func isLoopbackBrowserTarget(target *url.URL) bool {
	if target.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(target.Hostname())
	return ip != nil && ip.IsLoopback()
}

// The run is the primary parent so cancellation reaches dial contexts before its port can be reused.
type providerBrowserContext struct {
	context.Context
	request context.Context
}

func (c providerBrowserContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.request.Value(key)
}

func providerRequestContext(run, request context.Context) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if deadline, ok := request.Deadline(); ok {
		ctx, cancel = context.WithDeadline(run, deadline)
	} else {
		ctx, cancel = context.WithCancel(run)
	}
	stop := context.AfterFunc(request, cancel)
	if request.Err() != nil {
		cancel()
	}
	return providerBrowserContext{Context: ctx, request: request}, func() { stop(); cancel() }
}

func newProviderBrowserProxy(run *instance.Instance, target *url.URL, transport http.RoundTripper, limit int64, public *providerBrowserTarget) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme, request.Out.URL.Host = target.Scheme, target.Host
			request.Out.Host = request.In.Host
			scrubProviderBrowserRequest(request.Out.Context(), request.Out.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			scrubProviderBrowserResponse(response.Header)
			if err := confineProviderBrowserRedirect(response, target); err != nil {
				return err
			}
			return rewriteProviderBrowserRedirect(response, public)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if run.Context().Err() != nil {
				http.Error(w, "Provider run has stopped.", http.StatusGone)
				return
			}
			if r.Context().Err() != nil {
				return
			}
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "Upload exceeds the configured request limit.", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Provider browser upstream unavailable.", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := providerRequestContext(run.Context(), r.Context())
		defer cancel()
		if ctx.Err() != nil {
			http.NotFound(w, r)
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "Upload exceeds the configured request limit.", http.StatusRequestEntityTooLarge)
			return
		}
		forward := r.Clone(ctx)
		if forward.Body != nil {
			forward.Body = http.MaxBytesReader(w, forward.Body, limit)
		}
		proxy.ServeHTTP(w, forward)
	})
}

func confineProviderBrowserRedirect(response *http.Response, target *url.URL) error {
	raw := response.Header.Get("Location")
	if raw == "" {
		return nil
	}
	next, err := url.Parse(raw)
	if err != nil || next.User != nil || next.Opaque != "" {
		return errors.New("invalid provider redirect")
	}
	if next.IsAbs() || next.Host != "" {
		if next.Scheme != target.Scheme || next.Host != target.Host {
			return errors.New("provider redirect leaves its engine")
		}
		next.Scheme, next.Host = "", ""
		response.Header.Set("Location", next.String())
	}
	return nil
}

func scrubProviderBrowserRequest(ctx context.Context, headers http.Header) {
	scrubForUpstream(ctx, headers, backend.Engine())
	headers.Del("Cookie")
	headers.Del("Forwarded")
	headers.Del("X-Real-IP")
	for key := range headers {
		name := strings.ToLower(key)
		if strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "x-zzrouter-") || strings.HasPrefix(name, "sec-fetch-") {
			headers.Del(key)
		}
	}
}

func scrubProviderBrowserResponse(headers http.Header) {
	for _, name := range []string{"Set-Cookie", "Authorization", "Proxy-Authenticate", "WWW-Authenticate", "Service-Worker-Allowed"} {
		headers.Del(name)
	}
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), "access-control-") {
			headers.Del(key)
		}
	}
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
}
