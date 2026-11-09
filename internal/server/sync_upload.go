package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/common/download"
	"github.com/stperic/zzrouter/pkg/jobs"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

type incomingTransfer struct {
	upload   chan incomingUpload
	done     chan struct{}
	received chan error
	handle   jobs.Handle
	claimed  bool
}
type incomingUpload struct {
	body        io.ReadCloser
	contentType string
	ctx         context.Context
}

// HandleSyncUpload receives bytes for an accepted transfer; the worker never fetches them.
func (e *SyncExecutor) HandleSyncUpload(c *gin.Context) {
	if e.acceptUploads == nil || !e.acceptUploads() {
		NotFound(c, "node does not receive cluster uploads")
		return
	}
	e.inflightMu.Lock()
	transfer := e.incoming[c.Param("id")]
	if transfer == nil {
		e.inflightMu.Unlock()
		NotFound(c, "transfer is no longer pending")
		return
	}
	if transfer.claimed {
		e.inflightMu.Unlock()
		Conflict(c, "transfer upload already claimed")
		return
	}
	transfer.claimed = true
	e.inflightMu.Unlock()
	uploadCtx, cancel := context.WithCancel(c.Request.Context())
	stopJob := context.AfterFunc(transfer.handle.Context(), cancel)
	stopServer := context.AfterFunc(e.shutdownCtx(), cancel)
	body := &uploadBody{body: c.Request.Body, controller: http.NewResponseController(c.Writer), ctx: uploadCtx}
	interrupted := make(chan struct{})
	stopRead := context.AfterFunc(uploadCtx, func() { defer close(interrupted); body.abort() })
	defer func() {
		stopJob()
		stopServer()
		if !stopRead() {
			<-interrupted
		}
		cancel()
		_ = body.Close()
	}()
	upload := incomingUpload{body: body, contentType: c.GetHeader("Content-Type"), ctx: uploadCtx}
	select {
	case transfer.upload <- upload:
	case <-transfer.done:
		Conflict(c, "transfer is no longer pending")
		return
	case <-c.Request.Context().Done():
		return
	}
	select {
	case err, ok := <-transfer.received:
		if !ok || err != nil {
			ServiceUnavailable(c, "transfer failed; inspect its job")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "received", "job_id": transfer.handle.ID()})
	case <-c.Request.Context().Done():
	}
}

//nolint:contextcheck // Transfer lifetime intersects job/server context with incoming HTTP context.
func (e *SyncExecutor) receiveUpload(ctx context.Context, transfer *incomingTransfer, req SyncPullRequest, request metadata.DownloadRequest, progress modelsync.ProgressCallback) (*modelsync.Result, error) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	var upload incomingUpload
	select {
	case upload = <-transfer.upload:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("coordinator did not deliver the transfer")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(upload.ctx, cancel)
	defer stop()
	kind, params, err := mime.ParseMediaType(upload.contentType)
	if err != nil || kind != "multipart/mixed" || params["boundary"] == "" {
		return nil, errors.New("transfer requires multipart/mixed")
	}
	reader := multipart.NewReader(upload.body, params["boundary"])
	first, err := reader.NextPart()
	if err != nil {
		return nil, err
	}
	if first.FormName() != "manifest" {
		return nil, errors.New("transfer must begin with its manifest")
	}
	var manifest modelsync.SyncManifest
	manifestJSON, err := io.ReadAll(io.LimitReader(first, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(manifestJSON) > 1024*1024 {
		return nil, errors.New("transfer manifest exceeds size limit")
	}
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, err
	}
	if manifest.Model != request.Repo || manifest.Format != req.Format {
		return nil, fmt.Errorf("transfer manifest differs from accepted plan")
	}

	receiver := modelsync.NewClient(modelsync.ClientConfig{})
	result, err := receiver.ReceiveModel(ctx, req.SourceURL, request, &manifest, reader, progress)
	if err == nil && result.Success {
		// The multipart reader owns delimiter validation; MIME permits an epilogue.
		_, tailErr := io.CopyN(io.Discard, upload.body, 1024*1024+1)
		if !errors.Is(tailErr, io.EOF) {
			result.Success = false
			return result, errors.Join(errors.New("unexpected transfer suffix"), tailErr)
		}
	}
	return result, err
}

// uploadBody owns server-side read interruption. Request.Body.Close waits on active reads.
type uploadBody struct {
	mu         sync.Mutex
	body       io.ReadCloser
	controller *http.ResponseController
	ctx        context.Context
	aborted    bool
}

func (b *uploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.aborted || b.ctx.Err() != nil {
		b.mu.Unlock()
		return 0, context.Canceled
	}
	err := b.controller.SetReadDeadline(utils.Now().Add(download.BodyStallTimeout))
	b.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return b.body.Read(p)
}
func (b *uploadBody) abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aborted = true
	_ = b.controller.SetReadDeadline(utils.Now())
}
func (b *uploadBody) Close() error { b.abort(); return b.body.Close() }
