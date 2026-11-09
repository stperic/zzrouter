package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/stperic/common/download"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

func (s *DeploymentsService) startSyncUpload(id string, n DeploymentNode, request metadata.DownloadRequest, format string) bool {
	s.observerMu.Lock()
	if s.observerCtx == nil || s.observerCtx.Err() != nil {
		s.observerMu.Unlock()
		return false
	}
	ctx := s.observerCtx
	wg := s.observerWG
	wg.Add(1)
	s.observerMu.Unlock()
	go func() {
		defer wg.Done()
		if err := s.uploadModel(ctx, n, request, format); err != nil {
			s.cancelJob(ctx, n)
			s.trackingFailed(id, n, err)
		}
	}()
	return true
}
func (s *DeploymentsService) uploadModel(ctx context.Context, n DeploymentNode, request metadata.DownloadRequest, format string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := s.peerClient()
	if client == nil {
		return fmt.Errorf("coordinator transfer transport unavailable")
	}
	local := s.localTransferTarget(n.Source)
	security := modelsync.NewSecurity()

	if s.peerURL == nil {
		return fmt.Errorf("cluster endpoint resolver unavailable")
	}
	target := s.peerURL(n.Node)
	if target == "" {
		return fmt.Errorf("destination cluster endpoint unavailable")
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	// Keep the existing stall watchdog alive through request EOF and response headers.
	activity := download.NewStallReader(&transferCancellationBody{ReadCloser: reader, cancel: cancel})
	defer func() { _ = activity.Close() }()
	multipartWriter := multipart.NewWriter(writer)
	uploaded := make(chan error, 1)
	go func() {
		err := func() error {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="manifest"`)
			header.Set("Content-Type", "application/json")
			first, err := multipartWriter.CreatePart(header)
			if err != nil {
				return err
			}
			var manifest *modelsync.SyncManifest
			sourceClusterURL := ""
			if local {
				manifest, err = security.GenerateManifest(ctx, request, format)
			} else {
				sourceClusterURL = s.peerURL(n.Source)
				if sourceClusterURL == "" {
					err = fmt.Errorf("source cluster endpoint unavailable")
				}
				if err == nil {
					manifest, err = modelsync.NewClient(modelsync.ClientConfig{HTTPClient: client, RequestTimeout: download.BodyStallTimeout}).FetchManifest(ctx, sourceClusterURL, request, format)
				}
			}
			if err != nil {
				return err
			}
			if err := json.NewEncoder(first).Encode(manifest); err != nil {
				return err
			}
			for _, file := range manifest.Files {
				header := make(textproto.MIMEHeader)
				header.Set("Content-Disposition", "form-data; name="+strconv.Quote(file.Name))
				header.Set("Content-Type", "application/octet-stream")
				part, err := multipartWriter.CreatePart(header)
				if err != nil {
					return err
				}
				var input io.ReadCloser
				if local {
					path, err := security.ValidateModelFile(filepath.Join(request.Repo, file.Name))
					if err != nil {
						return err
					}
					input, err = os.Open(path)
					if err != nil {
						return err
					}
				} else {
					values := url.Values{"model": {request.Repo}, "format": {format}, "file": {file.Name}}
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceClusterURL+"/zzrouter/v1/internal/sync/file?"+values.Encode(), nil)
					if err != nil {
						return err
					}
					resp, err := client.Do(req)
					if err != nil {
						return err
					}
					if resp.StatusCode != 200 {
						_ = resp.Body.Close()
						return fmt.Errorf("source file returned status %d", resp.StatusCode)
					}
					input = download.NewStallReader(resp.Body)
				}
				copied, copyErr := io.Copy(part, io.LimitReader(input, file.Size+1))
				closeErr := input.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
				if copied != file.Size {
					return fmt.Errorf("source file size changed during transfer")
				}
			}
			return multipartWriter.Close()
		}()
		_ = writer.CloseWithError(err)
		uploaded <- err
	}()
	body := struct {
		io.Reader
		io.Closer
	}{Reader: activity, Closer: reader}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target+"/zzrouter/v1/internal/sync/deploy/"+url.PathEscape(n.JobID), body)
	if err == nil {
		req.Header.Set("Content-Type", "multipart/mixed; boundary="+multipartWriter.Boundary())
	}
	var resp *http.Response
	if err == nil {
		resp, err = client.Do(req)
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("destination upload returned status %d", resp.StatusCode)
		}
		_, drainErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1024*1024))
		_ = resp.Body.Close()
		if err == nil {
			err = drainErr
		}
	}
	cancel()
	_ = reader.CloseWithError(err)
	producerErr := <-uploaded
	if err != nil {
		return err
	}
	return producerErr
}

type transferCancellationBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *transferCancellationBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

func (s *DeploymentsService) localTransferTarget(selector string) bool {
	if s.nodeInfo == nil || s.resolveNodeURL == nil {
		return false
	}
	localURL := s.resolveNodeURL(s.nodeInfo.GetNodename())
	return localURL != "" && s.resolveNodeURL(selector) == localURL
}
