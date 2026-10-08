package server

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/routing"
)

type pathRecordingRouter struct {
	mockRouter
	path string
}

func (r *pathRecordingRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	r.path = req.Path
	return nil, errors.New("recorded")
}

// The coordinator asks its workers with the model's name as given: a
// variant's "+" is not turned into a space on the hop.
func TestListCompatibleNodesSendsTheModelAsGiven(t *testing.T) {
	router := &pathRecordingRouter{}
	svc := NewNodesService(nil, router, nil)
	_, _ = svc.ListCompatibleNodes(context.Background(), &ListCompatibleNodesRequest{Model: "qwen+agent", Provider: "llama cpp"})

	u, err := url.Parse(router.path)
	require.NoError(t, err)
	assert.Equal(t, "qwen+agent", u.Query().Get("model"))
	assert.Equal(t, "llama cpp", u.Query().Get("provider"))
}
