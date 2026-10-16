package download

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchBodyStall(t *testing.T) {
	for _, trickle := range []bool{false, true} {
		name := "stalled"
		if trickle {
			name = "trickling"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body, writer := io.Pipe()
				defer func() { _ = body.Close(); _ = writer.Close() }()
				var calls int
				client := &http.Client{Transport: fetchTestTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: body}, nil
				})}
				dir := t.TempDir()
				dest := filepath.Join(dir, "model")
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.test/file", nil)
				require.NoError(t, err)
				done := make(chan error, 1)
				go func() { _, err := Fetch(client, req, dest, Options{}); done <- err }()
				_, err = writer.Write([]byte("a"))
				require.NoError(t, err)
				synctest.Wait()
				if trickle {
					for range 4 {
						time.Sleep(BodyStallTimeout / 2)
						_, err := writer.Write([]byte("a"))
						require.NoError(t, err, "byte progress must renew the deadline beyond ten minutes overall")
						synctest.Wait()
					}
					require.NoError(t, writer.Close())
					require.NoError(t, <-done)
					data, err := os.ReadFile(dest)
					require.NoError(t, err)
					require.Equal(t, "aaaaa", string(data))
				} else {
					time.Sleep(BodyStallTimeout)
					synctest.Wait()
					select {
					case err := <-done:
						require.ErrorContains(t, err, "no data received for 10m0s")
						require.ErrorIs(t, err, context.DeadlineExceeded)
					default:
						t.Fatal("stalled body did not fail at the byte-silence deadline")
					}
					require.Empty(t, entries(t, dir), "a stall removes its partial without publishing")
				}
				require.Equal(t, 1, calls, "stalls must fail rather than retry")
			})
		})
	}
}

type emptyReadBody struct{ remaining int }

func (r *emptyReadBody) Read([]byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	r.remaining--
	time.Sleep(BodyStallTimeout / 2)
	return 0, nil
}
func (*emptyReadBody) Close() error { return nil }

func TestStallReaderEmptyReadsDoNotResetDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := NewStallReader(&emptyReadBody{remaining: 4})
		defer func() { _ = body.Close() }()
		_, err := io.ReadAll(body)
		require.ErrorContains(t, err, "no data received for 10m0s")
	})
}

type blockingCloseBody struct {
	io.ReadCloser
	started          atomic.Bool
	closing, release chan struct{}
}

func (b *blockingCloseBody) Close() error {
	if b.started.Swap(true) {
		return nil
	}
	err := b.ReadCloser.Close()
	close(b.closing)
	<-b.release
	return err
}

func TestStallReaderCloseWaitsForCleanup(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	body := &blockingCloseBody{ReadCloser: reader, closing: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(body.release) }) }
	defer release()
	guard := NewStallReader(body)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- guard.Close() }()
	<-body.closing
	go func() { second <- guard.Close() }()
	select {
	case <-second:
		t.Fatal("transfer cleanup returned while the watchdog's body Close was still running")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	require.NoError(t, <-first)
	require.NoError(t, <-second)
}
