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
	"net/http"

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
type Connector struct {
	modelsDir    string
	token        string // HF API token for gated models (optional)
	apiBase      string
	downloadBase string
}

// newGet creates a GET carrying Bearer auth when a token is configured.
// Every request the connector makes goes through it, so gated repos work
// for listings and downloads alike.
func (hfc *Connector) newGet(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
func NewConnector(modelsDir string) *Connector {
	return &Connector{
		modelsDir:    modelsDir,
		token:        search.FindToken(),
		apiBase:      huggingFaceAPIBase,
		downloadBase: huggingFaceDownloadBase,
	}
}
