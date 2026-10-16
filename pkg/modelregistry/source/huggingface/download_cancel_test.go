package huggingface

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestDownloadCancelWhileWaitingForMaterialization(t *testing.T) {
	hfc := &Connector{modelsDir: t.TempDir(), downloadBase: huggingFaceDownloadBase}
	dir := t.TempDir()
	req := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "model.gguf", Size: 7}}}}
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	defer release()
	var fetched atomic.Int32
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		fetched.Add(1)
		close(entered)
		select {
		case <-proceed:
			return targetResponse(r, "weights"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}))
	writerCtx, stopWriter := context.WithCancel(t.Context())
	defer stopWriter()
	writer := make(chan error, 1)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); writer <- hfc.Download(writerCtx, req, nil, CatalogDestination(dir)) }()
	t.Cleanup(func() { stopWriter(); release(); <-writerDone })
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never reached fetch")
	}
	before := onDisk(t, dir)
	manifestBefore, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := &materializationContext{Context: ctx, waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- hfc.Download(waiting, req, nil, CatalogDestination(dir)) }()
	select {
	case <-waiting.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never reached gate")
	}
	cancel()
	select {
	case err := <-waiter:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter is still blocked by the writer")
	}
	require.EqualValues(t, 1, fetched.Load())
	require.DirExists(t, dir, "waiter must preserve the active writer's directory")
	require.Equal(t, before, onDisk(t, dir), "waiter must not remove the writer's partial")
	after, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.Equal(t, manifestBefore, after, "waiter must not alter the writer's manifest")
	select {
	case err := <-writer:
		t.Fatalf("waiter stopped the active writer: %v", err)
	default:
	}
	release()
	select {
	case err := <-writer:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("writer failed to finish after waiter cancellation")
	}
	data, err := os.ReadFile(filepath.Join(dir, "model.gguf"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(data))
}

func TestDownloadCancelPreservesPendingAndHeldFiles(t *testing.T) {
	hfc := &Connector{modelsDir: t.TempDir(), downloadBase: huggingFaceDownloadBase}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "held.gguf"), []byte("held"), 0600))
	require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: "held.gguf", Size: 4, SHA256: "held-hash"}))
	req := metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{
		{TreeFileEntry: metadata.TreeFileEntry{Name: "held.gguf", Size: 4}},
		{TreeFileEntry: metadata.TreeFileEntry{Name: "arrived.json", Size: 7}},
		{TreeFileEntry: metadata.TreeFileEntry{Name: "pending.json", Size: 7}},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var fetched []string
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		name := filepath.Base(r.URL.Path)
		fetched = append(fetched, name)
		if name == "pending.json" {
			cancel()
			return nil, ctx.Err()
		}
		return targetResponse(r, "arrived"), nil
	}))
	require.ErrorIs(t, hfc.Download(ctx, req, nil, CatalogDestination(dir)), context.Canceled)
	require.Equal(t, []string{"arrived.json", "pending.json"}, fetched)
	manifest, err := integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	require.Equal(t, []string{"pending.json"}, manifest.Pending)
	require.Len(t, manifest.Files, 2)
	for name, content := range map[string]string{"held.gguf": "held", "arrived.json": "arrived"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, content, string(data))
	}
	require.NoFileExists(t, filepath.Join(dir, "pending.json"))
	require.Len(t, onDisk(t, dir), 3, "only held, completed and manifest remain")
	fetched = nil
	hfc.SetHTTPTransport(targetTransport(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, filepath.Base(r.URL.Path))
		return targetResponse(r, "pending"), nil
	}))
	require.NoError(t, hfc.Download(t.Context(), req, nil, CatalogDestination(dir)))
	require.Equal(t, []string{"pending.json"}, fetched, "repair fetches only the missing file")
	manifest, err = integrity.ReadManifest(dir)
	require.NoError(t, err)
	require.Empty(t, manifest.Pending)
	require.Len(t, manifest.Files, 3)
}
