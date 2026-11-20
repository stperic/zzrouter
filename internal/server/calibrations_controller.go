package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/calibration"
)

func (s *Server) createCalibration(c *gin.Context) {
	var req calibration.Request
	if !BindJSONStrict(c, &req) {
		return
	}
	result, err := (calibrationsService{router: s.cluster.router}).submit(c.Request.Context(), req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondAccepted(c, "Calibration accepted. Follow the job with zz_job.", result)
}
