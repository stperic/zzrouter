package connectivity

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type upgradeBody struct {
	bytes.Buffer
	closes int
}

func (b *upgradeBody) Close() error { b.closes++; return nil }

type upgradeTransport struct{ body io.ReadCloser }

func (t upgradeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{Request: r, StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header), Body: t.body}, nil
}

func TestStreamingMetricsPreserveBidirectionalBodyUntilClose(t *testing.T) {
	body := &upgradeBody{}
	manager := NewConnectionManager()
	manager.streamMetrics.transport = upgradeTransport{body}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://engine.invalid/upgrade", nil)
	require.NoError(t, err)
	resp, err := manager.GetStreamingClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	duplex, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok)
	require.EqualValues(t, 1, manager.GetStats().StreamActiveRequests)
	_, err = duplex.Write([]byte("ping"))
	require.NoError(t, err)
	require.Equal(t, "ping", body.String())
	read, err := io.ReadAll(duplex)
	require.NoError(t, err)
	require.Equal(t, "ping", string(read))
	require.EqualValues(t, 1, manager.GetStats().StreamActiveRequests, "EOF does not close an upgraded stream")
	require.NoError(t, duplex.Close())
	require.NoError(t, duplex.Close())
	require.Zero(t, manager.GetStats().StreamActiveRequests)
	require.Equal(t, 1, body.closes)
}
