package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
)

func TestClassifyRouteError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		node     string
		wantCode httperr.RouteErrorCode
		// wantDetail is a substring the detail must contain.
		wantDetail string
	}{
		{
			name:       "unicast transport failure names the node",
			err:        errors.New("dial tcp 192.0.2.10:9091: connect: connection refused"),
			node:       "worker-1",
			wantCode:   httperr.CodeNodeUnreachable,
			wantDetail: `node "worker-1" is not reachable`,
		},
		{
			name:       "empty node is a broadcast",
			err:        errors.New("no peers"),
			node:       "",
			wantCode:   httperr.CodeClusterUnavailable,
			wantDetail: "no cluster node was reachable",
		},
		{
			name:       "wildcard node is a broadcast",
			err:        errors.New("no peers"),
			node:       "*",
			wantCode:   httperr.CodeClusterUnavailable,
			wantDetail: "no cluster node was reachable",
		},
		{
			name:       "deadline on a named node",
			err:        fmt.Errorf("unicast: %w", context.DeadlineExceeded),
			node:       "worker-1",
			wantCode:   httperr.CodeUpstreamTimeout,
			wantDetail: `node "worker-1" did not respond`,
		},
		{
			name:       "deadline on a broadcast",
			err:        context.DeadlineExceeded,
			node:       "",
			wantCode:   httperr.CodeUpstreamTimeout,
			wantDetail: "no cluster node responded",
		},
		{
			name:       "cancellation is distinguished from timeout",
			err:        fmt.Errorf("unicast: %w", context.Canceled),
			node:       "worker-1",
			wantCode:   httperr.CodeUpstreamCanceled,
			wantDetail: "canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, detail := classifyRouteError(tt.err, tt.node)
			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, detail, tt.wantDetail)

			// The sanitization contract: the raw error text must never
			// reach the client-visible detail.
			assert.NotContains(t, detail, tt.err.Error())
		})
	}
}

func TestNewRoutedTransportErrorCarriesCodeAndNode(t *testing.T) {
	err := newRoutedTransportError(errors.New("connection refused"), "worker-1")

	require.NotNil(t, err.ProblemDetails)
	assert.Equal(t, http.StatusBadGateway, err.StatusCode)
	assert.Equal(t, string(httperr.CodeNodeUnreachable), err.ProblemDetails.Code)
	assert.Equal(t, "worker-1", err.ProblemDetails.Node)

	// Error() surfaces Detail, which is what the CLI and TUI render.
	assert.Contains(t, err.Error(), "worker-1")
}

func TestNewRoutedTransportErrorOmitsNodeOnBroadcast(t *testing.T) {
	for _, node := range []string{"", "*"} {
		err := newRoutedTransportError(errors.New("no peers"), node)
		require.NotNil(t, err.ProblemDetails)
		assert.Empty(t, err.ProblemDetails.Node, "broadcast blames no single node")
		assert.Equal(t, string(httperr.CodeClusterUnavailable), err.ProblemDetails.Code)
	}
}

// The project forbids em dashes in user-facing strings; these details are
// rendered verbatim in the CLI and TUI.
func TestRouteErrorDetailsHaveNoEmDash(t *testing.T) {
	for _, node := range []string{"worker-1", ""} {
		_, detail := classifyRouteError(errors.New("boom"), node)
		assert.False(t, strings.Contains(detail, "—"), "detail must not contain an em dash: %s", detail)
	}
}
