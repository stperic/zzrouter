package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/stperic/zzrouter/pkg/calibration"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/routing"
)

const internalCalibrationsPath = "/zzrouter/v1/internal/calibrations"

type calibrationAccepted struct {
	JobID string `json:"job_id"`
	Node  string `json:"node"`
}

func validateCalibrationRequest(req calibration.Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if node := constants.ParseNodeIdentifier(req.Node); node.Nodename == "" || node.IsWildcard() {
		return &calibration.FieldError{Field: "node", Message: "Select one node with zz_nodes."}
	}
	return nil
}

func (s *Server) submitCalibration(ctx context.Context, req calibration.Request) (*calibrationAccepted, error) {
	if err := validateCalibrationRequest(req); err != nil {
		return nil, err
	}
	selector := req.Node
	node := constants.ParseNodeIdentifier(selector).Nodename
	router := s.cluster.router
	if clusterRouter, ok := router.(*routing.ClusterAwareRouter); ok && clusterRouter.IsLocalNode(node) {
		req.Node = s.GetNodename()
	} else if endpoint := s.resolveClusterEndpoint(node); endpoint != nil {
		req.Node = cmp.Or(endpoint.NodeName, endpoint.Name, req.Node)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result, err := routeAndParse[calibrationAccepted](ctx, router, http.MethodPost, internalCalibrationsPath, selector, body)
	var routed *RoutedError
	if errors.As(err, &routed) && routed.StatusCode == http.StatusBadGateway && routed.ProblemDetails != nil {
		code := httperr.RouteErrorCode(routed.ProblemDetails.Code)
		if code == httperr.CodeNodeUnreachable || code == httperr.CodeUpstreamTimeout {
			return nil, newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable",
				"The calibration node did not answer. Check the node with zz_nodes.", code, selector)
		}
	}
	return result, err
}
