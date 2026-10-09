package clusternode

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestDialClientBoundsConnectionEstablishmentOnly(t *testing.T) {
	n, err := New(testConfig(t, Coordinator))
	require.NoError(t, err)
	t.Cleanup(n.pairingStore.Stop)
	client, err := n.DialClient("worker")
	require.NoError(t, err)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.DialContext)
	assert.Equal(t, constants.ClusterTLSHandshakeTimeout, transport.TLSHandshakeTimeout)
	assert.Zero(t, transport.ResponseHeaderTimeout)
	assert.Zero(t, client.Timeout)
}
