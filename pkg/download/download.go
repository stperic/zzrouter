// Package download transfers one URL to a file through a partial file,
// resuming after transient transport errors and publishing into place only when
// complete. Every large-file download zzRouter makes goes through Fetch.
package download

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/retry"
)

const (
	// maxRetries bounds resumes per file: enough for a CDN stream reset, not a dead host.
	maxRetries = 3
	// retryDelay and maxRetryDelay keep a resume quick next to a multi-GB transfer.
	retryDelay    = 250 * time.Millisecond
	maxRetryDelay = 2 * time.Second
	// PartialSuffix marks a file still being downloaded; model directory walks skip it.
	PartialSuffix = ".zzpart"
	// copyBufferSize is the read size per body chunk.
	copyBufferSize = 32 * 1024
)

// Options tunes one Fetch. The zero value downloads with no hash or limit.
type Options struct {
	NoReplace bool                    // atomically refuse an existing final entry
	Hash      bool                    // keep a running SHA256 over the whole file
	MaxSize   int64                   // refuse a larger file; 0 means unlimited
	Progress  func(done, total int64) // total is -1 when unknown; done rewinds on a restart
	Verify    func(Result) error      // the caller's check, before finalization
	Mode      fs.FileMode             // the final file's mode; 0 keeps the partial's 0600
	Exclusive bool                    // the caller serializes writers of dest, so an older partial of it is a dead transfer's
}

// Result describes a completed transfer.
type Result struct {
	Size        int64
	SHA256      string
	ResumedFrom int64 // the offset of the first resume, 0 if none
}

// CreatePartial creates the file dest is written into until it is complete,
// beside dest so the rename never crosses a filesystem, and uniquely named so
// two downloads of one file never write into each other.
func CreatePartial(dest string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*"+PartialSuffix)
}

// removeDeadPartials deletes what a killed transfer of dest left behind; only
// a partial's own name (prefix, random digits, suffix) is matched.
func removeDeadPartials(dest string) {
	prefix := "." + filepath.Base(dest) + "."
	entries, _ := os.ReadDir(filepath.Dir(dest))
	for _, e := range entries {
		rest, isDest := strings.CutPrefix(e.Name(), prefix)
		random, isPartial := strings.CutSuffix(rest, PartialSuffix)
		if isDest && isPartial && random != "" && strings.Trim(random, "0123456789") == "" {
			_ = os.Remove(filepath.Join(filepath.Dir(dest), e.Name()))
		}
	}
}

// Fetch downloads req to dest with client, which carries the caller's auth.
// The partial is removed on cancellation or any terminal failure.
func Fetch(client *http.Client, req *http.Request, dest string, opts Options) (result Result, err error) {
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return result, err
	}
	if opts.Exclusive {
		removeDeadPartials(dest)
	}
	out, err := CreatePartial(dest)
	if err != nil {
		return result, err
	}
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	if opts.Mode != 0 {
		if err = out.Chmod(opts.Mode); err != nil {
			return result, err
		}
	}
	var hasher hash.Hash
	if opts.Hash {
		hasher = sha256.New()
	}
	total := int64(-1)
	report := func() {
		if opts.Progress != nil {
			opts.Progress(result.Size, total)
		}
	}
	classify := retry.ClassifyNetwork(io.ErrShortWrite)
	backoff := retry.Backoff{Initial: retryDelay, Max: maxRetryDelay, Jitter: 0.2}
	for attempt := 0; ; attempt++ {
		var network bool
		network, err = transfer(client, req, out, hasher, &result, &total, opts.MaxSize, report)
		if req.Context().Err() != nil {
			return result, req.Context().Err()
		}
		if err == nil {
			break
		}
		var pathErr *os.PathError
		if !network || attempt >= maxRetries || errors.As(err, &pathErr) || classify(err) == retry.Stop {
			return result, err
		}
		if err = retry.Sleep(req.Context(), backoff.Delay(attempt)); err != nil {
			return result, err
		}
	}
	if hasher != nil {
		result.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	}
	if opts.Verify != nil {
		if err = opts.Verify(result); err != nil {
			return result, err
		}
	}
	if err = req.Context().Err(); err != nil {
		return result, err
	}
	if err = out.Sync(); err != nil {
		return result, err
	}
	if err = out.Close(); err != nil {
		return result, err
	}
	if opts.NoReplace {
		if err = os.Link(out.Name(), dest); err == nil {
			err = os.Remove(out.Name())
		}
	} else {
		err = os.Rename(out.Name(), dest)
	}
	return result, err
}

// partialSize checks that a 206 resumes exactly at offset and runs to the end
// of a file whose size agrees with known (negative when unknown), and returns
// that size.
func partialSize(resp *http.Response, offset, known int64) (int64, error) {
	value := resp.Header.Get("Content-Range")
	var start, end, size int64
	n, err := fmt.Sscanf(value, "bytes %d-%d/%d", &start, &end, &size)
	if err != nil || n != 3 || value != fmt.Sprintf("bytes %d-%d/%d", start, end, size) ||
		offset == 0 || start != offset || end < start || size <= end || end != size-1 ||
		(known >= 0 && size != known) || (resp.ContentLength >= 0 && resp.ContentLength != end-start+1) {
		return 0, fmt.Errorf("invalid content range %q for offset %d", value, offset)
	}
	return size, nil
}

func transfer(client *http.Client, req *http.Request, out *os.File, hasher hash.Hash, result *Result, total *int64, maxSize int64, report func()) (bool, error) {
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	request := req.Clone(ctx)
	request.Header.Del("Range")
	// Range offsets address wire bytes, so disable implicit decompression.
	if request.Header.Get("Accept-Encoding") == "" {
		request.Header.Set("Accept-Encoding", "identity")
	}
	if result.Size > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", result.Size))
	}
	resp, err := client.Do(request)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		*total = resp.ContentLength
		if result.Size > 0 {
			if err := out.Truncate(0); err != nil {
				return false, err
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return false, err
			}
			if hasher != nil {
				hasher.Reset()
			}
			result.Size, result.ResumedFrom = 0, 0
			report()
		}
	case http.StatusPartialContent:
		size, err := partialSize(resp, result.Size, *total)
		if err != nil {
			return false, err
		}
		*total = size
		if result.ResumedFrom == 0 {
			result.ResumedFrom = result.Size
		}
	default:
		return false, fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	if maxSize > 0 && *total > maxSize {
		return false, fmt.Errorf("file exceeds maximum size limit (%d bytes)", maxSize)
	}
	buf := make([]byte, copyBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if maxSize > 0 && int64(n) > maxSize-result.Size {
				return false, fmt.Errorf("file exceeds maximum size limit (%d bytes)", maxSize)
			}
			if *total >= 0 && int64(n) > *total-result.Size {
				return false, fmt.Errorf("body exceeds declared size")
			}
			written, err := out.Write(buf[:n])
			if err != nil {
				return false, err
			}
			if written != n {
				return false, io.ErrShortWrite
			}
			if hasher != nil {
				_, _ = hasher.Write(buf[:n])
			}
			result.Size += int64(n)
			report()
		}
		if readErr == io.EOF {
			if *total >= 0 && result.Size != *total {
				return true, io.ErrUnexpectedEOF
			}
			return false, nil
		}
		if readErr != nil {
			return true, readErr
		}
	}
}
