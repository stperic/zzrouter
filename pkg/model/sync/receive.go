package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"strings"

	"github.com/stperic/common/download"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

var errIncomingStream = errors.New("incoming model stream failed")

// ReceiveModel materializes a coordinator upload through the same manifest and lock owner as pulls.
func (c *Client) ReceiveModel(ctx context.Context, source string, request metadata.DownloadRequest, manifest *SyncManifest, reader *multipart.Reader, callback ProgressCallback) (*Result, error) {
	if err := c.validatePlan(manifest, request); err != nil {
		return nil, err
	}
	received := 0
	finish := func() error {
		if received != len(manifest.Files) {
			return nil
		}
		_, err := reader.NextPart()
		if !errors.Is(err, io.EOF) {
			return errors.Join(errors.New("unexpected transfer frame suffix"), err)
		}
		return nil
	}
	return c.materialize(ctx, source, request, manifest, callback, func(file FileChecksum, dest string, skip bool, progress func(int64, int64)) (copied int64, err error) {
		defer func() {
			if err != nil {
				err = errors.Join(errIncomingStream, err)
			}
		}()
		received++
		part, err := reader.NextPart()
		if err != nil {
			return 0, err
		}
		if part.FormName() != file.Name {
			return 0, fmt.Errorf("unexpected transfer file %q, want %q", part.FormName(), file.Name)
		}
		input := io.LimitReader(part, file.Size+1)
		if skip {
			copied, err := io.Copy(io.Discard, input)
			if err == nil && copied != file.Size {
				err = fmt.Errorf("skipped transfer size mismatch")
			}
			if err == nil {
				err = finish()
			}
			return 0, err
		}
		out, err := download.CreatePartial(dest)
		if err != nil {
			return 0, err
		}
		defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
		hash := sha256.New()
		writer := io.MultiWriter(out, hash)
		copied, err = io.Copy(writer, &receiveReader{ctx: ctx, reader: input, progress: sampleProgress(progress)})
		if err != nil {
			return copied, err
		}
		if copied != file.Size {
			return copied, fmt.Errorf("transfer size mismatch: got %d, want %d", copied, file.Size)
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), file.SHA256) {
			return copied, fmt.Errorf("transfer checksum mismatch")
		}
		if err := finish(); err != nil {
			return copied, err
		}
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if err := out.Sync(); err != nil {
			return copied, err
		}
		if err := out.Close(); err != nil {
			return copied, err
		}
		return copied, os.Rename(out.Name(), dest)
	})
}

type receiveReader struct {
	ctx      context.Context
	reader   io.Reader
	progress func(int64, int64)
	copied   int64
}

func (r *receiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.copied += int64(n)
	if r.progress != nil {
		r.progress(r.copied, 0)
	}
	return n, err
}
