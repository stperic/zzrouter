package server

import (
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

type calibrationsService struct{ router routing.Router }

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

func (s calibrationsService) submit(ctx context.Context, req calibration.Request) (*calibrationAccepted, error) {
	if err := validateCalibrationRequest(req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result, err := routeAndParse[calibrationAccepted](ctx, s.router, http.MethodPost, internalCalibrationsPath, req.Node, body)
	var routed *RoutedError
	if errors.As(err, &routed) && routed.StatusCode == http.StatusBadGateway && routed.ProblemDetails != nil {
		code := httperr.RouteErrorCode(routed.ProblemDetails.Code)
		if code == httperr.CodeNodeUnreachable || code == httperr.CodeUpstreamTimeout {
			return nil, newRouteProblemError(http.StatusServiceUnavailable, "Service Unavailable",
				"The calibration node did not answer. Check the node with zz_nodes.", code, req.Node)
		}
	}
	return result, err
}
