package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func incomingManifest(data string) *SyncManifest {
	return &SyncManifest{Model: "org/model", Format: "gguf", TotalSize: int64(len(data)), Files: []FileChecksum{{Name: "weights.gguf", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}}}
}
func TestReceiveOversizedFrameDoesNotDrainUnboundedInput(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	release, writerDone, receiverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		part, err := multipartWriter.CreateFormField("weights.gguf")
		if err == nil {
			_, _ = part.Write([]byte("TOOLONG"))
		}
		<-release
		_ = multipartWriter.Close()
		_ = writer.Close()
	}()
	result := make(chan error, 1)
	go func() {
		defer close(receiverDone)
		_, err := NewClient(ClientConfig{}).ReceiveModel(t.Context(), "source", metadata.DownloadRequest{Repo: "org/model", Force: true}, incomingManifest("GGUF"), multipart.NewReader(reader, multipartWriter.Boundary()), nil)
		result <- err
	}()
	t.Cleanup(func() { close(release); _ = reader.Close(); _ = writer.Close(); <-writerDone; <-receiverDone })
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("receiver drained a rejected frame instead of stopping")
	}
	_, err := os.Stat(filepath.Join(root, "org/model/weights.gguf"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
func TestReceiveChecksumFailurePreservesExistingFile(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	dir := filepath.Join(root, "org/model")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "weights.gguf")
	require.NoError(t, os.WriteFile(path, []byte("KEEP"), 0o600))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormField("weights.gguf")
	require.NoError(t, err)
	_, err = part.Write([]byte("FAIL"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, err = NewClient(ClientConfig{}).ReceiveModel(context.Background(), "source", metadata.DownloadRequest{Repo: "org/model", Force: true}, incomingManifest("GGUF"), multipart.NewReader(&body, writer.Boundary()), nil)
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("KEEP"), data)
	partials, err := filepath.Glob(filepath.Join(dir, "*.zzpart"))
	require.NoError(t, err)
	require.Empty(t, partials)
}
