package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestFetchFinalizationPolicy(t *testing.T) {
	for _, noReplace := range []bool{false, true} {
		t.Run(fmt.Sprint(noReplace), func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "model")
			client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: 7, Body: io.NopCloser(strings.NewReader("weights"))}, nil
			})}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.test/file", nil)
			require.NoError(t, err)
			_, err = Fetch(client, req, dest, Options{NoReplace: noReplace, Verify: func(Result) error {
				return os.WriteFile(dest, []byte("operator"), 0600)
			}})
			want := "weights"
			if noReplace {
				require.ErrorIs(t, err, fs.ErrExist)
				want = "operator"
			} else {
				require.NoError(t, err, "zero-value policy retains replacing finalization")
			}
			data, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Equal(t, want, string(data))
			require.Len(t, entries(t, dir), 1, "completed or refused transfers leave no partial")
		})
	}
}

func TestFetchResumesInterruptedBody(t *testing.T) {
	const body = "abcdefghijklmnopqrstuvwxyz"
	digest := sha256.Sum256([]byte(body))
	wantSum := hex.EncodeToString(digest[:])
	for _, tc := range []struct {
		name        string
		failures    int
		ignoreRange bool
		refuse      bool
		wantCalls   int32
		wantErr     string
		bodyErr     error
		badRange    bool
		corrupt     bool
		connectAt   int32
	}{
		{name: "resume twice", failures: 2, wantCalls: 3},
		{name: "resume reconnect fails once", failures: 1, connectAt: 2, wantCalls: 3},
		{name: "first connect fails once", connectAt: 1, wantCalls: 2},
		{name: "ignored range restarts", failures: 1, ignoreRange: true, wantCalls: 2},
		{name: "ignored range restarts twice", failures: 2, ignoreRange: true, wantCalls: 3},
		{name: "bounded failures", failures: 4, wantCalls: 4, wantErr: "unexpected EOF"},
		{name: "refusal stops", refuse: true, wantCalls: 1, wantErr: "status 403"},
		{name: "refusal after interruption stops", failures: 1, refuse: true, wantCalls: 2, wantErr: "status 403"},
		{name: "HTTP2 CANCEL", failures: 1, wantCalls: 2, bodyErr: http2.StreamError{StreamID: 7, Code: http2.ErrCodeCancel}},
		{name: "HTTP2 reset", failures: 1, wantCalls: 2, bodyErr: http2.StreamError{StreamID: 7, Code: http2.ErrCodeInternal}},
		{name: "connection reset", failures: 1, wantCalls: 2, bodyErr: fmt.Errorf("read: %w", syscall.ECONNRESET)},
		{name: "invalid range stops", failures: 1, badRange: true, wantCalls: 2, wantErr: "invalid content range"},
		{name: "resumed checksum mismatch stops", failures: 1, corrupt: true, wantCalls: 2, wantErr: "sha256 mismatch"},
		{name: "local IO error stops", failures: 1, bodyErr: &os.PathError{Op: "write", Path: "partial", Err: syscall.ENOSPC}, wantCalls: 1, wantErr: "no space left"},
		{name: "short write stops", failures: 1, bodyErr: io.ErrShortWrite, wantCalls: 1, wantErr: "short write"},
		{name: "deadline stops", failures: 1, bodyErr: context.DeadlineExceeded, wantCalls: 1, wantErr: "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			received := 0
			client := &http.Client{Transport: fetchTestTransport(func(r *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				offset := 0
				wantOffset := received
				if wantOffset > 0 {
					_, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &offset)
					assert.NoError(t, err)
					assert.Equal(t, wantOffset, offset)
				} else {
					assert.Empty(t, r.Header.Get("Range"))
				}
				if call == tc.connectAt {
					return nil, syscall.ECONNRESET
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body[offset:]))}
				if tc.refuse && int(call) > tc.failures {
					resp.StatusCode = http.StatusForbidden
					return resp, nil
				}
				if tc.ignoreRange {
					offset = 0
				}
				resp.ContentLength = int64(len(body) - offset)
				if offset > 0 {
					resp.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
					if tc.badRange {
						resp.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset+1, len(body)-1, len(body)))
					}
					resp.StatusCode = http.StatusPartialContent
				}
				if int(call) <= tc.failures {
					bodyErr := tc.bodyErr
					if bodyErr == nil {
						bodyErr = io.ErrUnexpectedEOF
					}
					received = offset + 3
					resp.Body = io.NopCloser(fetchTestBody{strings.NewReader(body[offset : offset+3]), bodyErr})
				} else {
					content := body[offset:]
					if tc.corrupt {
						content = strings.ToUpper(content)
					}
					resp.Body = io.NopCloser(strings.NewReader(content))
				}
				return resp, nil
			})}
			dir := t.TempDir()
			dest := filepath.Join(dir, "model.gguf")
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.test/model.gguf", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer secret")
			var done int64
			var updates []int64
			result, err := Fetch(client, req, dest, Options{
				Hash:     true,
				Progress: func(n, _ int64) { done = n; updates = append(updates, n) },
				Verify: func(result Result) error {
					if result.SHA256 != wantSum {
						return fmt.Errorf("sha256 mismatch")
					}
					return nil
				},
			})
			sum := result.SHA256
			assert.Equal(t, tc.wantCalls, calls.Load())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, entries(t, dir))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, wantSum, sum)
			got, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, body, string(got))
			assert.EqualValues(t, len(body), done)
			assert.Len(t, entries(t, dir), 1)
			assert.Empty(t, req.Header.Get("Range"))
			if tc.ignoreRange {
				assert.Contains(t, updates, int64(0))
			}
		})
	}
}

func TestFetchCancellationRemovesPartial(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(fetchTestBody{
			strings.NewReader("prefix"), io.ErrUnexpectedEOF,
		})}, nil
	})}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://hub.test/model.gguf", nil)
	require.NoError(t, err)
	_, err = Fetch(client, req, filepath.Join(dir, "model.gguf"), Options{
		Progress: func(int64, int64) { cancel() },
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, calls.Load())
	assert.Empty(t, entries(t, dir))
}

type fetchTestTransport func(*http.Request) (*http.Response, error)

func (f fetchTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fetchTestBody struct {
	io.Reader
	err error
}

func (b fetchTestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = b.err
	}
	return n, err
}

func entries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return entries
}

func TestFetchRejectsBadRanges(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		length      int64
	}{
		{"missing", "", 3},
		{"wrong start", "bytes 2-5/6", 4},
		{"end before start", "bytes 3-2/6", -1},
		{"end past total", "bytes 3-6/6", 4},
		{"changed total", "bytes 3-6/7", 4},
		{"short range", "bytes 3-4/6", 2},
		{"wrong length", "bytes 3-5/6", 2},
		{"trailing garbage", "bytes 3-5/6 junk", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 200, ContentLength: 6, Body: io.NopCloser(fetchTestBody{strings.NewReader("abc"), io.ErrUnexpectedEOF})}, nil
				}
				return &http.Response{StatusCode: 206, ContentLength: tc.length, Header: http.Header{"Content-Range": {tc.value}}, Body: io.NopCloser(strings.NewReader("def"))}, nil
			})}
			dir := t.TempDir()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.test/file", nil)
			require.NoError(t, err)
			_, err = Fetch(client, req, filepath.Join(dir, "file"), Options{})
			require.ErrorContains(t, err, "invalid content range")
			assert.Equal(t, 2, calls)
			assert.Empty(t, entries(t, dir))
		})
	}
}

func TestFetchTerminalFailuresPreserveDestination(t *testing.T) {
	for _, tc := range []struct {
		name            string
		length, maxSize int64
		verify          bool
		want            string
	}{
		{name: "declared size limit", length: 6, maxSize: 5, want: "maximum size limit"},
		{name: "stream size limit", length: -1, maxSize: 5, want: "maximum size limit"},
		{name: "oversized body", length: 5, want: "body exceeds declared size"},
		{name: "verification", length: 6, verify: true, want: "verification refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader("abcdef"))}, nil
			})}
			dir := t.TempDir()
			dest := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(dest, []byte("existing"), 0o600))
			opts := Options{MaxSize: tc.maxSize}
			if tc.verify {
				opts.Verify = func(Result) error { return fmt.Errorf("verification refused") }
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.test/file", nil)
			require.NoError(t, err)
			_, err = Fetch(client, req, dest, opts)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, 1, calls)
			data, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, "existing", string(data))
			assert.Len(t, entries(t, dir), 1)
		})
	}
}

func TestFetchRenameErrorRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, ContentLength: 6, Body: io.NopCloser(io.MultiReader(strings.NewReader("abc"), strings.NewReader("def")))}, nil
	})}
	dest := filepath.Join(dir, "file")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.test/file", nil)
	require.NoError(t, err)
	_, err = Fetch(client, req, dest, Options{Verify: func(Result) error {
		return os.Mkdir(dest, 0o700)
	}})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Len(t, entries(t, dir), 1)
	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestTransferLocalWriteErrorIsTerminal(t *testing.T) {
	out, err := CreatePartial(filepath.Join(t.TempDir(), "file"))
	require.NoError(t, err)
	require.NoError(t, out.Close())
	client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 3, Body: io.NopCloser(strings.NewReader("abc"))}, nil
	})}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.test/file", nil)
	require.NoError(t, err)
	result, total := Result{}, int64(-1)
	network, err := transfer(client, req, out, nil, &result, &total, 0, func() {})
	require.ErrorIs(t, err, os.ErrClosed)
	assert.False(t, network)
	assert.Zero(t, result.Size)
}

func TestFetchExclusiveRemovesOnlyDeadPartialsOfDest(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(fmt.Sprint(exclusive), func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "model.safetensors")
			dead := filepath.Join(dir, ".model.safetensors.1697557971"+PartialSuffix)
			kept := []string{
				filepath.Join(dir, ".model.safetensors.index.json.42"+PartialSuffix), // another file's partial
				filepath.Join(dir, ".tokenizer.json.7"+PartialSuffix),
				filepath.Join(dir, ".model.safetensors.x1"+PartialSuffix), // not a CreateTemp name
			}
			for _, p := range append([]string{dead}, kept...) {
				require.NoError(t, os.WriteFile(p, []byte("killed"), 0600))
			}
			client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: 7, Body: io.NopCloser(strings.NewReader("weights"))}, nil
			})}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.test/file", nil)
			require.NoError(t, err)
			_, err = Fetch(client, req, dest, Options{Exclusive: exclusive})
			require.NoError(t, err)
			if exclusive {
				require.NoFileExists(t, dead, "a writer holding dest's lock owns its older partials")
			} else {
				require.FileExists(t, dead, "without the lock a partial may be a live concurrent transfer")
			}
			for _, p := range kept {
				require.FileExists(t, p)
			}
		})
	}
}
