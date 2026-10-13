package port

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPinnedPortRefusesOccupiedListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	pool := mustNewPool(t, Range{port, port})
	_, err = pool.Allocate()
	require.Error(t, err)
	_, err = pool.Allocate(port)
	require.Error(t, err)
	require.Empty(t, pool.ListAllocated())
	require.NoError(t, listener.Close())
	allocated, err := pool.Allocate()
	require.NoError(t, err)
	require.Equal(t, port, allocated)
}
