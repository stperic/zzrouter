package mesh

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestNodeTransportPreservesBidirectionalUpgrades(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	require.NoError(t, local.SetDeadline(utils.Now().Add(2*time.Second)))
	require.NoError(t, remote.SetDeadline(utils.Now().Add(2*time.Second)))
	_, client, _ := nodeTestState(t, nodeTestTransport(func(r *http.Request) (*http.Response, error) {
		return nodeTestResponse(r, http.StatusSwitchingProtocols, local), nil
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, nodeTestCluster+"/upgrade", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	upgraded, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok, "the canonical cluster transport must retain the upgraded connection's writer")
	done := make(chan error, 1)
	go func() {
		input := make([]byte, 4)
		_, err := io.ReadFull(remote, input)
		if err == nil && string(input) != "ping" {
			err = fmt.Errorf("unexpected upgraded input %q", input)
		}
		if err == nil {
			_, err = remote.Write([]byte("pong"))
		}
		done <- err
	}()
	_, err = upgraded.Write([]byte("ping"))
	require.NoError(t, err)
	output := make([]byte, 4)
	_, err = io.ReadFull(upgraded, output)
	require.NoError(t, err)
	require.Equal(t, "pong", string(output))
	require.NoError(t, <-done)
}
