package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stretchr/testify/require"
)

// Returning bytes with the cancellation forces one last progress callback before
// Close blocks physical unwind. No scheduler timing decides the assertion.
type zeroWeightBody struct {
	ctx                     context.Context
	ready, closing, release chan struct{}
	cancel                  bool
}

func (b *zeroWeightBody) Read(p []byte) (int, error) {
	close(b.ready)
	if b.cancel {
		<-b.ctx.Done()
		return copy(p, "part"), b.ctx.Err()
	}
	<-b.release
	return copy(p, "GGUF"), io.EOF
}
func (b *zeroWeightBody) Close() error {
	close(b.closing)
	if b.cancel {
		<-b.release
	}
	return nil
}

func zeroWeightNodes(t *testing.T, location string) (target, reader *Server, dir, neighbor string) {
	t.Helper()
	target, reader, dir = mutationNodes(t, location)
	target.modelsWatcher.Stop()
	if reader != target {
		reader.modelsWatcher.Stop()
	}
	require.NoError(t, os.RemoveAll(dir))
	neighbor = filepath.Join(filepath.Dir(dir), "neighbor", "keep.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(neighbor), 0700))
	require.NoError(t, os.WriteFile(neighbor, []byte("keep"), 0600))
	require.NoError(t, target.publishCatalog())
	reader.services.Model.router = mutationRouter{engine: buildInternalEngine(target)}
	require.False(t, reader.model.Cache.HasModel("org/model"), "warm cached absence without watcher assistance")
	return
}

func zeroWeightTransfer(t *testing.T, target *Server, kind string, cancelled bool, ready, closing, release chan struct{}, publish func() error) string {
	t.Helper()
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			manifest := modelsync.SyncManifest{Model: "org/model", Format: "gguf", TotalSize: 4,
				Files: []modelsync.FileChecksum{{Name: "model.gguf", Size: 4, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("GGUF")))}}}
			raw, err := json.Marshal(manifest)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 4, Request: r,
			Body: &zeroWeightBody{ctx: r.Context(), ready: ready, closing: closing, release: release, cancel: cancelled}}, nil
	})}
	if kind == "hf" {
		e := target.newDeploymentsExecutor()
		e.publishCatalog = publish
		e.registry.GetHuggingFaceConnector().SetHTTPTransport(client.Transport)
		return startCancelableDownload(t, e, t.Context(), "huggingface").JobID
	}
	e := NewSyncExecutor(func() *http.Client { return client }, 0, func() context.Context { return t.Context() }, publish, nil).WithJobs(target.jobs)
	h, err := target.jobs.StartDetached(jobs.KindSync, "", nil)
	require.NoError(t, err)
	go e.executeSyncPull(SyncPullRequest{Model: "org/model", Format: "gguf"}, h, "zero-weight-sync", modelsync.NewClient(modelsync.ClientConfig{HTTPClient: client}), "https://source.test")
	return h.ID()
}

func zeroWeightJob(t *testing.T, target *Server, id string) jobs.Event {
	t.Helper()
	sub, err := target.jobs.Subscribe(id, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			require.True(t, ok, "job stream closed before terminal outcome")
			if ev.Phase.IsTerminal() {
				return ev
			}
		case <-deadline:
			t.Fatal("job never settled")
		}
	}
}

func zeroWeightDelete(t *testing.T, reader *Server, node, transport string) int {
	t.Helper()
	args := map[string]any{"model": "org/model", "node": node, "registry": "huggingface", "source_id": "org/model"}
	if transport == "REST" {
		response := makeAuthRequest(t, reader, http.MethodDelete, "/zzrouter/v1/models", TestAdminKey, args)
		if response.Code == 200 {
			require.Contains(t, string(response.Body), `"deleted":1`)
		}
		return response.Code
	}
	wire := mcpWireResponse(t, reader, mcpWireRequest(t, "tools/call", "2025-06-18", map[string]any{"name": "zz_model_delete", "arguments": args}), 200)
	result := wire["result"].(map[string]any)
	fields := result["structuredContent"].(map[string]any)
	if result["isError"] == true {
		return int(fields["status"].(float64))
	}
	require.EqualValues(t, 1, fields["deleted"])
	return 200
}

func TestCancelledCatalogDownloadPublishesIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct{ location, kind string }{{"coordinator", "hf"}, {"worker", "hf"}, {"worker", "sync"}} {
		for _, transport := range []string{"REST", "MCP"} {
			t.Run(tc.location+"/"+tc.kind+"/"+transport, func(t *testing.T) {
				target, reader, dir, neighbor := zeroWeightNodes(t, tc.location)
				ready, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				unwind := func() { once.Do(func() { close(release) }) }
				t.Cleanup(unwind)
				publishing, publishRelease := make(chan struct{}), make(chan struct{})
				var publishOnce sync.Once
				finishPublication := func() { publishOnce.Do(func() { close(publishRelease) }) }
				t.Cleanup(finishPublication)
				id := zeroWeightTransfer(t, target, tc.kind, true, ready, closing, release, func() error {
					unlock, lockErr := integrity.AcquireMaterialization(t.Context(), dir, false)
					if lockErr != nil {
						return lockErr
					}
					unlock()
					entries, readErr := os.ReadDir(dir)
					if readErr != nil {
						return readErr
					}
					for _, entry := range entries {
						if strings.HasSuffix(entry.Name(), ".zzpart") {
							return errors.New("partial survived physical unwind")
						}
					}
					close(publishing)
					<-publishRelease
					return target.publishCatalog()
				})
				awaitDownloadSignal(t, ready)
				require.NoFileExists(t, filepath.Join(dir, "model.gguf"))
				api := downloadCancelAPI(t, target.newDeploymentsExecutor(), tc.location == "worker")
				w := httptest.NewRecorder()
				api.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/zzrouter/v1/jobs/"+id, nil))
				require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
				awaitDownloadSignal(t, closing)
				ev, err := target.jobs.Get(id)
				require.NoError(t, err)
				require.False(t, ev.Phase.IsTerminal(), "terminal status must follow physical unwind")
				unwind()
				awaitDownloadSignal(t, publishing)
				ev, err = target.jobs.Get(id)
				require.NoError(t, err)
				require.False(t, ev.Phase.IsTerminal(), "terminal status must follow catalog publication")
				finishPublication()
				ev = zeroWeightJob(t, target, id)
				require.Equal(t, jobs.PhaseFailed, ev.Phase)
				require.Equal(t, "cancelled", ev.Meta["transfer_status"])
				listed := makeAuthRequest(t, reader, "GET", "/zzrouter/v1/models?model=org%2Fmodel", TestAdminKey, nil)
				require.Equal(t, 200, listed.Code, string(listed.Body))
				var listing struct {
					Data []map[string]any `json:"data"`
				}
				require.NoError(t, json.Unmarshal(listed.Body, &listing))
				require.Len(t, listing.Data, 1, string(listed.Body))
				require.Equal(t, true, listing.Data[0]["incomplete"])
				require.Equal(t, tc.location, listing.Data[0]["node"])
				wire := mcpWireResponse(t, reader, mcpWireRequest(t, "tools/call", "2025-06-18", map[string]any{"name": "zz_models", "arguments": map[string]any{"model": "org/model"}}), 200)
				result := wire["result"].(map[string]any)
				require.NotEqual(t, true, result["isError"], result)
				require.EqualValues(t, 1, result["structuredContent"].(map[string]any)["total"])
				deleteNode := tc.location
				if tc.location == "worker" && transport == "REST" {
					deleteNode = "" // unique-holder routing discovers the worker
				}
				require.Equal(t, 200, zeroWeightDelete(t, reader, deleteNode, transport), "first deletion after terminal must succeed")
				require.NoDirExists(t, dir)
				require.FileExists(t, neighbor)
				require.Equal(t, 404, zeroWeightDelete(t, reader, deleteNode, transport))
			})
		}
	}
}

func TestDeleteRefusesWhileMaterializing(t *testing.T) {
	for _, kind := range []string{"hf", "sync"} {
		t.Run(kind, func(t *testing.T) {
			target, reader, dir, neighbor := zeroWeightNodes(t, "worker")
			ready, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			t.Cleanup(resume)
			id := zeroWeightTransfer(t, target, kind, true, ready, closing, release, target.publishCatalog)
			awaitDownloadSignal(t, ready)
			require.NoError(t, target.publishCatalog()) // make the live incomplete identity addressable
			manifest, err := os.ReadFile(filepath.Join(dir, integrity.ManifestFileName))
			require.NoError(t, err)
			response := makeAuthRequest(t, reader, "DELETE", "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": "org/model", "node": "worker", "registry": "huggingface", "source_id": "org/model"})
			require.Equal(t, 409, response.Code, string(response.Body))
			require.Contains(t, string(response.Body), `"code":"download_in_progress"`)
			current, err := os.ReadFile(filepath.Join(dir, integrity.ManifestFileName))
			require.NoError(t, err)
			require.Equal(t, manifest, current)
			require.FileExists(t, neighbor)
			// Cancellation after publication retains the same repository identity,
			// allowing an identical selected-delete request after writer release.
			require.NoError(t, target.jobs.Cancel(id))
			awaitDownloadSignal(t, closing)
			resume()
			require.Equal(t, jobs.PhaseFailed, zeroWeightJob(t, target, id).Phase)
			require.Equal(t, 200, zeroWeightDelete(t, reader, "worker", "REST"))
			require.NoDirExists(t, dir)
			require.FileExists(t, neighbor)
			nextReady, nextClosing, nextRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
			close(nextRelease)
			next := zeroWeightTransfer(t, target, kind, false, nextReady, nextClosing, nextRelease, target.publishCatalog)
			require.Equal(t, jobs.PhaseDone, zeroWeightJob(t, target, next).Phase)
			require.FileExists(t, filepath.Join(dir, "model.gguf"), "a later download may recreate deleted files")
		})
	}
}

func TestCancelledDownloadProgressDoesNotEndJob(t *testing.T) {
	e := downloadTargetExecutor(t)
	a := beginTestDownload(t, e, "cancel-progress")
	require.NoError(t, e.jobs.Cancel(a.job.ID()))
	e.updateDownloadJob(a, "cancel-progress", "downloading", "weights", 0, 1, 50, 4, 8, 1)
	ev, err := e.jobs.Get(a.job.ID())
	require.NoError(t, err)
	require.False(t, ev.Phase.IsTerminal())
	e.updateDownloadJob(a, "cancel-progress", "failed", context.Canceled.Error(), 0, 1, 50, 4, 8, 0)
	ev, err = e.jobs.Get(a.job.ID())
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseFailed, ev.Phase)
}

// Keep publication failure distinct from cancellation in the job evidence.
func TestCancelledHFPublicationFailureKeepsCancellation(t *testing.T) {
	e := downloadTargetExecutor(t)
	e.publishCatalog = func() error { return errors.New("publication fixture") }
	e.registry.GetHuggingFaceConnector().SetHTTPTransport(mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	id := startCancelableDownload(t, e, t.Context(), "huggingface").JobID
	require.NoError(t, e.jobs.Cancel(id))
	ev := waitJob(t, e.jobs, id)
	require.Equal(t, "cancelled", ev.Meta["transfer_status"])
	require.Contains(t, ev.Err, "publication fixture")
}

func TestFailedCatalogTransfersPublishOnce(t *testing.T) {
	for _, kind := range []string{"hf", "sync"} {
		for _, publicationFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/publicationFails=%t", kind, publicationFails), func(t *testing.T) {
				target, reader, _, _ := zeroWeightNodes(t, "worker")
				calls := 0
				publish := func() error {
					calls++
					if err := target.publishCatalog(); err != nil {
						return err
					}
					if publicationFails {
						return errors.New("publication fixture")
					}
					return nil
				}
				client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
					body, status := "upstream fixture", 400
					if strings.HasSuffix(r.URL.Path, "/manifest") {
						body = fmt.Sprintf(`{"model":"org/model","format":"gguf","files":[{"name":"model.gguf","size":4,"sha256":"%x"}]}`, sha256.Sum256([]byte("GGUF")))
						status = 200
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				var id string
				if kind == "hf" {
					e := target.newDeploymentsExecutor()
					e.publishCatalog = publish
					e.registry.GetHuggingFaceConnector().SetHTTPTransport(client.Transport)
					id = startCancelableDownload(t, e, t.Context(), "huggingface").JobID
				} else {
					h, err := target.jobs.StartDetached(jobs.KindSync, "", nil)
					require.NoError(t, err)
					id = h.ID()
					e := NewSyncExecutor(func() *http.Client { return client }, 0, func() context.Context { return t.Context() }, publish, nil).WithJobs(target.jobs)
					go e.executeSyncPull(SyncPullRequest{Model: "org/model", Format: "gguf"}, h, "failed-sync", modelsync.NewClient(modelsync.ClientConfig{HTTPClient: client}), "https://source.test")
				}
				ev := zeroWeightJob(t, target, id)
				require.Equal(t, jobs.PhaseFailed, ev.Phase)
				require.NotEqual(t, "cancelled", ev.Meta["transfer_status"])
				require.Equal(t, 1, calls, "each settled outcome publishes exactly once")
				if publicationFails {
					require.Contains(t, ev.Err, "publication fixture")
				}
				var retained bool
				for _, model := range reader.model.Cache.GetAllModels() {
					retained = retained || (model.Name == "org/model" && model.Node == "worker" && model.Incomplete)
				}
				require.True(t, retained, "failed transfer must invalidate cached absence")
				require.False(t, reader.model.Cache.HasModel("org/model"), "incomplete files remain ineligible for inference")
			})
		}
	}
}
