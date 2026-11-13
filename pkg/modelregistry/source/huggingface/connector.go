// Package huggingface is the HuggingFace-specific source connector
// under pkg/modelregistry/source. It covers scan, show, tree,
// download, and delete operations against the HuggingFace Hub and the
// local on-disk model cache.
//
// Invariant: this package must not import pkg/modelregistry. The
// orchestrator depends on us, not the other way around — see
// pkg/modelregistry/source/internal_import_guard_test.go.
package huggingface

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
)

// HuggingFace API endpoints shared across the connector's per-concern
// files (scan, show, tree, download).
const (
	huggingFaceAPIBase      = "https://huggingface.co/api/models/"
	huggingFaceDownloadBase = "https://huggingface.co/"
)

// httpTimeout is the timeout for large model downloads. Sourced from
// pkg/constants so the operator can tune it centrally.
var httpTimeout = constants.HTTPDownloadTimeout

// Connector handles all HuggingFace-specific operations — scan, show,
// tree, download, delete. Per-concern logic lives in the sibling files
// in this package; this file only defines the struct + auth plumbing.
type credentialHeaderNames struct{}

type Connector struct {
	connections  *backend.Resolver
	modelsDir    string
	token        string // HF API token for gated models (optional)
	apiBase      string
	downloadBase string
	transport    atomic.Pointer[http.RoundTripper]
}

// SetHTTPTransport replaces this connector's transport. Nil uses net/http's
// default; each request's client retains its snapshot through retries.
func (hfc *Connector) SetHTTPTransport(transport http.RoundTripper) {
	hfc.transport.Store(&transport)
}

func (hfc *Connector) httpClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many upstream redirects")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return http.ErrUseLastResponse
		}
		if len(via) > 0 && (!strings.EqualFold(req.URL.Host, via[0].URL.Host) || req.URL.Scheme != via[0].URL.Scheme) {
			req.Header.Del("Authorization")
			if names, ok := via[0].Context().Value(credentialHeaderNames{}).([]string); ok {
				for _, name := range names {
					req.Header.Del(name)
				}
			}
		}
		return nil
	}}
	if transport := hfc.transport.Load(); transport != nil {
		client.Transport = *transport
	}
	return client
}

// newGet creates a GET carrying Bearer auth when a token is configured.
// Every request the connector makes goes through it, so gated repos work
// for listings and downloads alike.
func (hfc *Connector) newGet(ctx context.Context, rawURL string) (*http.Request, error) {
	if hfc.connections != nil {
		target, ok := hfc.connections.Target("huggingface")
		if !ok {
			return nil, backend.ErrConnectionUnavailable
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return nil, errors.New("invalid registry URL")
		}
		req, err := target.NewRequest(ctx, http.MethodGet, parsed.RequestURI(), nil)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(req.Header))
		for name := range req.Header {
			names = append(names, name)
		}
		return req.WithContext(context.WithValue(ctx, credentialHeaderNames{}, names)), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if hfc.token != "" {
		req.Header.Set("Authorization", "Bearer "+hfc.token)
	}
	return req, nil
}

// NewConnector creates a Connector bound to the given on-disk models
// directory. The HF API token is auto-discovered from env vars
// (HF_TOKEN, HUGGING_FACE_TOKEN) or the standard HF CLI cache at
// ~/.cache/huggingface/token — empty token is fine, public endpoints
// still work without auth.
func NewConnector(modelsDir string, resolvers ...*backend.Resolver) *Connector {
	var connections *backend.Resolver
	var token string
	if len(resolvers) > 0 {
		connections = resolvers[0]
	} else {
		token = search.FindToken()
	}
	return &Connector{connections: connections,
		modelsDir:    modelsDir,
		token:        token,
		apiBase:      huggingFaceAPIBase,
		downloadBase: huggingFaceDownloadBase,
	}
}
