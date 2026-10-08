package instance

import (
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStdinPipeConcurrentLaunchStop(t *testing.T) {
	for range 100 {
		inst := NewInstance("id", "provider", "model", 0, 0, 0)
		reader, writer := io.Pipe()
		var workers sync.WaitGroup
		workers.Add(2)
		start := make(chan struct{})
		go func() { defer workers.Done(); <-start; inst.SetStdinPipe(writer) }()
		go func() {
			defer workers.Done()
			<-start
			for range 100 {
				_ = inst.HasStdinPipe()
				inst.CloseStdinPipe()
			}
		}()
		close(start)
		workers.Wait()
		require.True(t, inst.HasStdinPipe())
		inst.CloseStdinPipe()
		require.NoError(t, reader.Close())
	}
}
