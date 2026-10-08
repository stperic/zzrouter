package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// MockGitHubServer creates a mock GitHub API server for testing
type MockGitHubServer struct {
	Server   *httptest.Server
	Releases []githubRelease
}

// NewMockGitHubServer creates a new mock GitHub API server
func NewMockGitHubServer(t *testing.T) *MockGitHubServer {
	t.Helper()

	mock := &MockGitHubServer{
		Releases: []githubRelease{},
	}

	mock.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handle /repos/{owner}/{repo}/releases
		if strings.Contains(r.URL.Path, "/releases") {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(mock.Releases); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		http.NotFound(w, r)
	}))

	t.Cleanup(func() {
		mock.Server.Close()
	})

	return mock
}

// AddReleaseWithVersion is a convenience method to add a release with common defaults
func (m *MockGitHubServer) AddReleaseWithVersion(tagName string, prerelease bool) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}

	assetName := "zzrouter-" + osName + "-" + arch + "." + ext

	release := githubRelease{
		TagName:     tagName,
		Name:        "Release " + tagName,
		Body:        "Release notes for " + tagName,
		PublishedAt: "2024-01-15T10:00:00Z",
		HTMLURL:     "https://github.com/stperic/zzrouter/releases/tag/" + tagName,
		Prerelease:  prerelease,
		Draft:       false,
		Assets: []githubAsset{
			{
				Name:               assetName,
				Size:               1024000,
				BrowserDownloadURL: "https://github.com/stperic/zzrouter/releases/download/" + tagName + "/" + assetName,
				ContentType:        "application/gzip",
			},
			{
				Name:               "checksums.txt",
				Size:               256,
				BrowserDownloadURL: "https://github.com/stperic/zzrouter/releases/download/" + tagName + "/checksums.txt",
				ContentType:        "text/plain",
			},
			{
				Name:               "checksums.txt.sig",
				Size:               128,
				BrowserDownloadURL: "https://github.com/stperic/zzrouter/releases/download/" + tagName + "/checksums.txt.sig",
				ContentType:        "application/octet-stream",
			},
			{
				Name:               "checksums.txt.pem",
				Size:               512,
				BrowserDownloadURL: "https://github.com/stperic/zzrouter/releases/download/" + tagName + "/checksums.txt.pem",
				ContentType:        "application/x-pem-file",
			},
		},
	}
	m.Releases = append(m.Releases, release)
}
