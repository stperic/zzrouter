package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderBrowserRequestOriginBoundary(t *testing.T) {
	origin, err := url.Parse("https://comfy.example.com")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, method, host, supplied, site, mode, dest, upgrade string
		allowed                                                 bool
	}{
		{name: "entry navigation", method: "GET", site: "cross-site", mode: "navigate", dest: "document", allowed: true},
		{name: "OAuth return navigation", method: "GET", site: "same-site", mode: "navigate", dest: "document", allowed: true},
		{name: "same origin mutation", method: "POST", supplied: origin.String(), site: "same-origin", allowed: true},
		{name: "same origin upgrade", method: "GET", supplied: origin.String(), site: "same-origin", upgrade: "websocket", allowed: true},
		{name: "same origin asset", method: "GET", site: "same-origin", allowed: true},
		{name: "wrong Host", method: "GET", host: "admin.example.com"},
		{name: "forwarded Host cannot help", method: "GET", host: "localhost:9092"},
		{name: "missing mutation Origin", method: "POST"},
		{name: "null Origin", method: "POST", supplied: "null"},
		{name: "related hostname mutation", method: "POST", supplied: "https://other.example.com", site: "same-site"},
		{name: "upgrade needs Origin", method: "GET", upgrade: "websocket"},
		{name: "cross-site asset", method: "GET", site: "cross-site", mode: "no-cors", dest: "image"},
		{name: "related hostname asset", method: "GET", site: "same-site", mode: "no-cors", dest: "image"},
		{name: "cross-site navigation cannot mutate", method: "POST", supplied: origin.String(), site: "cross-site", mode: "navigate", dest: "document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, origin.String()+"/runs/local/run/", nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			r.Header.Set("Origin", tc.supplied)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("Sec-Fetch-Mode", tc.mode)
			r.Header.Set("Sec-Fetch-Dest", tc.dest)
			r.Header.Set("Upgrade", tc.upgrade)
			r.Header.Set("X-Forwarded-Host", origin.Host)
			require.Equal(t, tc.allowed, providerBrowserRequestAllowed(r, origin))
		})
	}
	r := httptest.NewRequest(http.MethodPost, origin.String()+"/", nil)
	r.Header["Origin"] = []string{origin.String(), origin.String()}
	require.False(t, providerBrowserRequestAllowed(r, origin))
}

func TestProviderBrowserTabTargetsAndEscapedSuffix(t *testing.T) {
	for _, raw := range []string{"/runs/node-a/run-a/view/a%2Fb?filename=a%2Bb.png", "/runs/node-b/run-b/ws?clientId=one"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		target, ok := parseProviderBrowserTarget(u)
		require.True(t, ok)
		require.Equal(t, raw, target.prefix+target.suffix.String())
	}
	for _, raw := range []string{"/runs/node/run", "/runs//run/", "/runs/node/../", "/runs/%2e%2e/run/", "/runs/node%2Frun/run/", "/runs/node/run%5Cother/", "/runs/node/run%00/", "/runs/*/run/", "/other/node/run/"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		_, ok := parseProviderBrowserTarget(u)
		require.False(t, ok, raw)
	}
}

func TestProviderBrowserRedirectStaysInSelectedRun(t *testing.T) {
	u, err := url.Parse("/runs/node/run/dir/page?old=one")
	require.NoError(t, err)
	target, ok := parseProviderBrowserTarget(u)
	require.True(t, ok)
	for raw, want := range map[string]string{
		"/view?filename=a%2Bb.png": "/runs/node/run/view?filename=a%2Bb.png",
		"../index.html":            "/runs/node/run/index.html",
		"?next=one":                "/runs/node/run/dir/page?next=one",
	} {
		got, err := providerBrowserRedirect(raw, target)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"https://evil.example/", "//evil.example/", "javascript:alert(1)", "/%2e%2e/logout", "/\\evil.example/", "http://engine:8188/path"} {
		_, err := providerBrowserRedirect(raw, target)
		require.Error(t, err, raw)
	}
	engine, err := url.Parse("http://127.0.0.1:8188")
	require.NoError(t, err)
	response := &http.Response{Header: http.Header{"Location": {engine.String() + "/index.html"}}}
	require.NoError(t, confineProviderBrowserRedirect(response, engine))
	require.NoError(t, rewriteProviderBrowserRedirect(response, &target))
	require.Equal(t, "/runs/node/run/index.html", response.Header.Get("Location"))
	response.Header.Set("Location", "https://evil.example/")
	require.Error(t, confineProviderBrowserRedirect(response, engine))
}
