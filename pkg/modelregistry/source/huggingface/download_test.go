package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

var vision = map[string][]string{"vision": {"mmproj-F16.gguf", "mmproj-*.gguf"}}

// fakeHub serves a repository's listing and files the way huggingface.co
// does, counting every file it serves.
type fakeHub struct {
	files   map[string]string
	served  atomic.Int32
	listed  atomic.Int32
	corrupt string // a file served with bytes that fail its hash
}

func (h *fakeHub) start(t *testing.T) *Connector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			h.listed.Add(1)
			var sib []map[string]any
			for name, body := range h.files {
				sum := sha256.Sum256([]byte(body))
				sib = append(sib, map[string]any{"rfilename": name, "size": len(body),
					"lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(body)}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": sib})
			return
		}
		name := r.URL.Path[strings.Index(r.URL.Path, "/resolve/main/")+len("/resolve/main/"):]
		h.served.Add(1)
		if name == h.corrupt {
			_, _ = w.Write([]byte("tampered"))
			return
		}
		_, _ = w.Write([]byte(h.files[name]))
	}))
	t.Cleanup(srv.Close)
	return &Connector{modelsDir: t.TempDir(), apiBase: srv.URL + "/api/models/", downloadBase: srv.URL + "/"}
}

func TestDownloadUsesResolvedFileSetWithoutRelisting(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"model.gguf": "weights", "other.gguf": "other"}}
	hfc := hub.start(t)
	req := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "model.gguf", Size: 7}}}}
	require.NoError(t, hfc.Download(t.Context(), req, nil))
	assert.Zero(t, hub.listed.Load())
	assert.Equal(t, int32(1), hub.served.Load())
	req.Files = []metadata.DownloadFile{}
	require.NoError(t, hfc.Download(t.Context(), req, nil))
	assert.Zero(t, hub.listed.Load())
	assert.Equal(t, int32(1), hub.served.Load())
}

func onDisk(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestPlan(t *testing.T) {
	listing := []metadata.TreeFileEntry{
		{Name: "README.md"}, {Name: "M-Q4_K_M.gguf"}, {Name: "mmproj-BF16.gguf"}, {Name: "mmproj-F16.gguf"},
		{Name: "Q8_0/M-Q8_0-00001-of-00002.gguf"}, {Name: "Q8_0/M-Q8_0-00002-of-00002.gguf"},
	}
	names := func(ps []planned) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.file.Name+"="+p.feature)
		}
		return out
	}
	cases := []struct {
		name string
		req  metadata.DownloadRequest
		want []string
	}{
		{"a quant tag never picks a feature file", metadata.DownloadRequest{Weights: "F16", Features: vision},
			nil},
		{"a shard brings its whole set", metadata.DownloadRequest{Weights: "Q8_0", Features: vision},
			[]string{"Q8_0/M-Q8_0-00001-of-00002.gguf=", "Q8_0/M-Q8_0-00002-of-00002.gguf="}},
		{"a wanted feature comes along, by preference", metadata.DownloadRequest{Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}},
			[]string{"M-Q4_K_M.gguf=", "mmproj-F16.gguf=vision"}},
		{"the whole repo leaves unwanted feature files out", metadata.DownloadRequest{Features: vision},
			[]string{"README.md=", "M-Q4_K_M.gguf=", "Q8_0/M-Q8_0-00001-of-00002.gguf=", "Q8_0/M-Q8_0-00002-of-00002.gguf="}},
		{"an exact non-weights file still downloads", metadata.DownloadRequest{Weights: "README.md"},
			[]string{"README.md="}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := plan(listing, c.req)
			if c.want == nil {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, names(got))
		})
	}
	_, err := plan(listing, metadata.DownloadRequest{Weights: "Q4_K_M", Want: []string{"vision"}})
	assert.ErrorContains(t, err, "not one this engine declares")
	_, err = plan(listing[:2], metadata.DownloadRequest{Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}})
	assert.ErrorContains(t, err, "no file for feature")
}

// Deploying a feature onto a downloaded model fetches only the feature's
// file, keeps the weights, and records what the file is for.
func TestDownload_AddsWithoutReplacing(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"M-Q4_K_M.gguf": "weights", "mmproj-F16.gguf": "projector"}}
	hfc := hub.start(t)
	ctx := context.Background()

	require.NoError(t, hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M", Features: vision}, nil))
	dir := filepath.Join(hfc.modelsDir, "org", "m")
	assert.Equal(t, []string{".zzrouter-manifest.json", "M-Q4_K_M.gguf"}, onDisk(t, dir))

	require.NoError(t, hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}}, nil))
	assert.Equal(t, int32(2), hub.served.Load(), "the weights were not fetched again")
	assert.Equal(t, []string{".zzrouter-manifest.json", "M-Q4_K_M.gguf", "mmproj-F16.gguf"}, onDisk(t, dir))

	m, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	features := map[string]string{}
	for _, f := range m.Files {
		features[f.RelativePath] = f.Feature
	}
	assert.Equal(t, map[string]string{"M-Q4_K_M.gguf": "", "mmproj-F16.gguf": "vision"}, features)
}

// A file truncated on disk is fetched again; a fetch that fails its hash
// leaves no partial behind and the files already there untouched.
func TestDownload_RepairsAndCleansUp(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"M-Q4_K_M.gguf": "weights", "mmproj-F16.gguf": "projector"}}
	hfc := hub.start(t)
	ctx := context.Background()
	req := metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M"}
	require.NoError(t, hfc.Download(ctx, req, nil))
	dir := filepath.Join(hfc.modelsDir, "org", "m")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "M-Q4_K_M.gguf"), []byte("wei"), 0o600))
	require.NoError(t, hfc.Download(ctx, req, nil))
	got, err := os.ReadFile(filepath.Join(dir, "M-Q4_K_M.gguf"))
	require.NoError(t, err)
	assert.Equal(t, "weights", string(got))

	hub.corrupt = "mmproj-F16.gguf"
	err = hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}}, nil)
	require.Error(t, err)
	assert.Equal(t, []string{".zzrouter-manifest.json", "M-Q4_K_M.gguf"}, onDisk(t, dir))
}

// A feature's file that arrived before it was asked for, in a whole-repo
// download, is tagged when the feature is deployed, without fetching it
// again, and from then on is never weights.
func TestDownload_TagsAFeatureFileAlreadyHeld(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"M-Q4_K_M.gguf": "weights", "mmproj-F16.gguf": "projector"}}
	hfc := hub.start(t)
	ctx := context.Background()
	require.NoError(t, hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m"}, nil))
	require.Equal(t, int32(2), hub.served.Load())

	require.NoError(t, hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}}, nil))
	assert.Equal(t, int32(2), hub.served.Load(), "nothing was fetched again")
	m, err := integrity.ReadManifest(filepath.Join(hfc.modelsDir, "org", "m"))
	require.NoError(t, err)
	for _, f := range m.Files {
		if f.RelativePath == "mmproj-F16.gguf" {
			assert.Equal(t, "vision", f.Feature)
		}
	}
}

func TestDownload_ForceRefetchesSelectedSet(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"M-Q4_K_M.gguf": "weights", "mmproj-F16.gguf": "projector"}}
	hfc := hub.start(t)
	ctx := context.Background()
	require.NoError(t, hfc.Download(ctx, metadata.DownloadRequest{Repo: "org/m"}, nil))
	require.Equal(t, int32(2), hub.served.Load())
	req := metadata.DownloadRequest{Repo: "org/m", Weights: "Q4_K_M", Features: vision, Force: true}
	require.NoError(t, hfc.Download(ctx, req, nil))
	assert.Equal(t, int32(3), hub.served.Load(), "force fetches weights, leaving the unselected projector alone")
	req.Want = []string{"vision"}
	require.NoError(t, hfc.Download(ctx, req, nil))
	assert.Equal(t, int32(5), hub.served.Load(), "force fetches weights and the selected projector")
	m, err := integrity.ReadManifest(filepath.Join(hfc.modelsDir, "org", "m"))
	require.NoError(t, err)
	for _, f := range m.Files {
		if f.RelativePath == "mmproj-F16.gguf" {
			assert.Equal(t, "vision", f.Feature)
		}
	}
}

func TestPlanMissingDefaultFeatureIsOptional(t *testing.T) {
	listing := []metadata.TreeFileEntry{{Name: "M-Q4_K_M.gguf"}}
	req := metadata.DownloadRequest{Repo: "org/model", Weights: "Q4_K_M", Features: vision, Want: []string{"vision"}, Optional: []string{"vision"}}
	files, err := Plan(listing, req)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "M-Q4_K_M.gguf", files[0].Name)
	req.Optional = nil
	_, err = Plan(listing, req)
	assert.ErrorContains(t, err, "no file for feature")
}

// A repository Hugging Face does not serve is recognisable as such, so a
// caller can tell a mistyped model from a registry that did not answer.
func TestGetRepoFiles_MissingRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	hfc := &Connector{apiBase: srv.URL + "/api/models/"}
	_, err := hfc.GetRepoFiles(context.Background(), "org/missing")
	require.ErrorIs(t, err, ErrModelNotFound)
}

func TestConcurrentFeatureDeploysShareWeights(t *testing.T) {
	hub := &fakeHub{files: map[string]string{"model.gguf": "weights", "mmproj-F16.gguf": "projector"}}
	hfc := hub.start(t)
	first := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "model.gguf", Size: 7}}}}
	second := first
	second.Files = append(append([]metadata.DownloadFile{}, first.Files...), metadata.DownloadFile{TreeFileEntry: metadata.TreeFileEntry{Name: "mmproj-F16.gguf", Size: 9}, Feature: "vision"})
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- hfc.Download(t.Context(), first, func(_ string, _, _ int64) {
			select {
			case <-started:
			default:
				close(started)
				<-finish
			}
		})
	}()
	<-started
	waiting := make(chan struct{})
	go func() { close(waiting); done <- hfc.Download(t.Context(), second, nil) }()
	<-waiting
	select {
	case err := <-done:
		close(finish)
		require.NoError(t, <-done)
		t.Fatalf("second materializer finished while weights owner was blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	assert.EqualValues(t, 2, hub.served.Load(), "weights fetched once, projector fetched once")
	manifest, err := integrity.ReadManifest(filepath.Join(hfc.modelsDir, "org/model"))
	require.NoError(t, err)
	require.NotNil(t, manifest)
	require.Len(t, manifest.Files, 2)
	assert.FileExists(t, filepath.Join(hfc.modelsDir, "org/model/mmproj-F16.gguf"))
}
