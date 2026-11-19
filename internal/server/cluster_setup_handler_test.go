package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestWorkerSetupUsesClusterIdentityAndRequiresAdmin(t *testing.T) {
	s := coordTestServer(t)
	s.clusterHandlers.advertiseURL = "https://coordinator.example:443/"
	s.clusterHandlers.config.Cluster.BindPort = 9191
	for _, authenticated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/cluster/pairing/setup", nil)
		if authenticated {
			r.Header.Set("X-API-Key", TestAdminKey)
		}
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		if !authenticated {
			require.Equal(t, http.StatusUnauthorized, w.Code)
			continue
		}
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response struct {
			Data workerSetupGuide `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, "https://coordinator.example:443", response.Data.CoordinatorURL)
		require.Equal(t, s.cluster.listener.CAFingerprint(), response.Data.CAFingerprint)
		require.Equal(t, 9191, response.Data.WorkerPort)
		require.NotContains(t, w.Body.String(), TestAdminKey)
		require.Len(t, response.Data.Platforms, 3)
		for _, platform := range response.Data.Platforms {
			for _, installation := range platform.Installations {
				require.Contains(t, installation.InstallCommand, "9191")
				require.Contains(t, installation.PairCommand, "--secure")
				require.Contains(t, installation.PairCommand, "https://coordinator.example:443")
			}
		}
	}
}

func TestWorkerSetupRejectsLocalOnlyCluster(t *testing.T) {
	s := coordTestServer(t)
	s.clusterHandlers.advertiseURL = "https://127.0.0.1:9091"
	r := httptest.NewRequest(http.MethodGet, "/zzrouter/v1/cluster/pairing/setup", nil)
	r.Header.Set("X-API-Key", TestAdminKey)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "console can remain local-only")
}

func TestWorkerGuideQuotesPublicIdentityAndPinsRelease(t *testing.T) {
	guide := buildWorkerSetupGuide("https://worker's-$name.example:443", "sha256:literal'$value", 9091, version.MustParseVersion("2.3.4-rc1+build"))
	require.Equal(t, "https://github.com/stperic/zzrouter/releases/tag/v2.3.4-rc1", guide.DownloadURL)
	for _, platform := range guide.Platforms {
		for _, installation := range platform.Installations {
			if platform.OS == "windows" {
				require.Contains(t, installation.PairCommand, "'https://worker''s-$name.example:443'")
				require.Contains(t, installation.PairCommand, "'sha256:literal''$value'")
			} else {
				require.Contains(t, installation.PairCommand, "'https://worker'\"'\"'s-$name.example:443'")
				require.Contains(t, installation.PairCommand, "'sha256:literal'\"'\"'$value'")
			}
		}
	}
	require.False(t, strings.Contains(buildWorkerSetupGuide("https://coord:9091", "pin", 9091, version.MustParseVersion("0.0.0-dev")).DownloadURL, "/tag/"))
}

func TestPairingErrorPreservesDeliveryUncertainty(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		err := errors.New("connection failed")
		want := http.StatusBadRequest
		if uncertain {
			err = errors.Join(clusternode.ErrEnrollmentUncertain, err)
			want = http.StatusServiceUnavailable
		}
		respondPairingAcceptError(c, err)
		require.Equal(t, want, w.Code)
	}
}
