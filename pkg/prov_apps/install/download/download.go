// Package download owns provider-install HTTP fetches: streamed file
// download with progress callback and host allowlist validation.
// Host validation goes through pkg/security.ValidateDownloadURL so the
// allowlist is honored uniformly.
package download

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	filedownload "github.com/stperic/zzrouter/pkg/download"
	"github.com/stperic/zzrouter/pkg/security"
)

// fetchTimeout is the maximum duration for a single file download.
// Large model binaries and pip wheels may take considerable time.
const fetchTimeout = 30 * time.Minute

// fetchTransport is shared across all download.Fetch calls. The standard
// library transport defaults are too aggressive for installs over slow
// networks (Tailscale tailnet, transcontinental cloud links): 10s TLS
// handshake and 30s connect were observed timing out on a Vultr Windows
// worker reaching github.com via the GitHub CDN, even though PowerShell
// (using OS-level TCP/TLS) completed the same handshake in ~1s.
//
// Install steps fail loudly when these timeouts trip — the failure mode
// reads as "TLS handshake timeout" with no clue that the underlying
// network is fine. Raised values absorb tailnet jitter without making
// genuine outages drag on (the outer fetchTimeout still bounds the
// total request).
// NewClient returns an *http.Client wired to the install-side
// transport (raised TLSHandshakeTimeout / dial timeout for slow-network
// resilience). Adjacent installer code (resolveLatestVersion,
// VerifyChecksumFromURL) calls this instead of building its own client
// with default timeouts.
//
// Returning a fresh *http.Client per call — but reusing the shared
// transport — keeps connection pooling intact while preventing callers
// from mutating the transport's TLSClientConfig / Proxy out from under
// every other install. The transport itself is unexported.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: fetchTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("install download exceeded redirect limit")
			}
			return security.ValidateDownloadURL(req.URL.String(), AllowedHosts)
		},
	}
}

var fetchTransport http.RoundTripper = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   30 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// AllowedHosts is the list of hosts from which downloads are permitted.
var AllowedHosts = []string{
	"github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"github-releases.githubusercontent.com",
	"pypi.org",
	"files.pythonhosted.org",
	"ollama.com",
}

// Result contains the outcome of a fetch.
type Result struct {
	FilePath string
	Size     int64
}

// Options bounds a fetch; the zero value preserves unbounded binary downloads.
type Options struct {
	MaxSize int64
}

// FetchForProvider is Fetch plus a per-provider host pin check (defense
// in depth alongside config-load validation). Installer code that knows
// which provider it serves should prefer this entrypoint so a poisoned
// URL that somehow bypassed config validation still cannot reach the
// network.
func FetchForProvider(ctx context.Context, provider, rawURL, destDir string,
	progress func(downloaded, total int64, percent int),
	options Options,
) (*Result, error) {
	if err := security.ValidateInstallHost(provider, rawURL); err != nil {
		return nil, err
	}
	return Fetch(ctx, rawURL, destDir, progress, options)
}

// Fetch fetches a file from rawURL to destDir, validating the host.
// progress is called with (downloaded, total, percent).
func Fetch(ctx context.Context, rawURL, destDir string, progress func(downloaded, total int64, percent int), options Options) (*Result, error) {
	if err := security.ValidateDownloadURL(rawURL, AllowedHosts); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(destDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create download directory: %w", err)
	}

	// Extract filename from URL
	parsed, _ := url.Parse(rawURL)
	filename := filepath.Base(parsed.Path)
	if filename == "" || filename == "." {
		filename = "download"
	}
	destPath := filepath.Join(destDir, filename)

	client := NewClient(fetchTimeout)
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}

	result, err := filedownload.Fetch(client, req, destPath, filedownload.Options{
		MaxSize: options.MaxSize,
		Mode:    0o640,
		Progress: func(done, total int64) {
			if progress != nil && total > 0 {
				progress(done, total, int(done*100/total))
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &Result{FilePath: destPath, Size: result.Size}, nil
}
