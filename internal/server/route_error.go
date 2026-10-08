package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Cluster-routing failures are sanitized before they reach a client: the
// raw transport error can carry Go type names and internal host:port
// detail, so it is logged rather than returned. Sanitizing to a bare
// "upstream routing failed", though, left operators with a 502 and no way
// to tell an offline node from an unpaired one from a slow one.
//
// classifyRouteError closes that gap without reopening the leak: it maps
// the failure onto a closed enum (httperr.RouteErrorCode) and names the
// node that was targeted. Both travel as RFC 9457 extensions, so agents
// branch on Code and humans read Detail.

// broadcastNodeTarget is the node value meaning "every peer"; routeAndParse
// receives it as an empty string or an explicit wildcard.
const broadcastNodeTarget = "*"

// classifyRouteError maps a routing transport error to a wire code and a
// human-readable detail. node is the request's target ("" or "*" for a
// broadcast). The returned detail never embeds err.
func classifyRouteError(err error, node string) (httperr.RouteErrorCode, string) {
	broadcast := node == "" || node == broadcastNodeTarget

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		if broadcast {
			return httperr.CodeUpstreamTimeout, "no cluster node responded before the request deadline"
		}
		return httperr.CodeUpstreamTimeout, fmt.Sprintf("node %q did not respond before the request deadline", node)

	case errors.Is(err, context.Canceled):
		return httperr.CodeUpstreamCanceled, "the request was canceled before the upstream responded"

	case broadcast:
		return httperr.CodeClusterUnavailable,
			"no cluster node was reachable for this request; check node health with GET /zzrouter/v1/nodes"

	default:
		return httperr.CodeNodeUnreachable, fmt.Sprintf(
			"node %q is not reachable: it may be offline, or no longer paired to this coordinator; "+
				"check node health with GET /zzrouter/v1/nodes", node)
	}
}

// newRouteProblemError builds a *RoutedError carrying the closed-enum code
// and the targeted node alongside the sanitized detail. node is omitted
// from the payload for broadcasts, where no single node is at fault.
func newRouteProblemError(status int, title, detail string, code httperr.RouteErrorCode, node string) *RoutedError {
	pd := utils.NewProblemDetails(status, title, detail, "")
	pd.Code = string(code)
	if node != "" && node != broadcastNodeTarget {
		pd.Node = node
	}
	return &RoutedError{
		StatusCode:     status,
		ProblemDetails: pd,
	}
}

// newRoutedTransportError is the 502 returned when router.Route itself
// fails, classified by cause.
func newRoutedTransportError(err error, node string) *RoutedError {
	code, detail := classifyRouteError(err, node)
	return newRouteProblemError(http.StatusBadGateway, "Bad Gateway", detail, code, node)
}
