package mesh

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestEndpointCopyOwnsProviderDiagnostics(t *testing.T) {
	code := 17
	ep := &Endpoint{Snapshot: EndpointSnapshot{HealthReport: HealthReport{Apps: []prov_apps.LocalProviderInfo{{
		Formats: []string{"gguf"}, Service: &prov_apps.ProviderServiceStatus{LastExit: &prov_apps.ProviderServiceExit{
			Reason: "failed", Detail: &instance.FailureInfo{ExitCode: &code, ErrorTail: []string{"original"}},
		}},
	}}}}}
	copied := copyEndpoint(ep)
	copied.Snapshot.Apps[0].Formats[0] = "changed"
	copied.Snapshot.Apps[0].Service.LastExit.Reason = "changed"
	copied.Snapshot.Apps[0].Service.LastExit.Detail.ErrorTail[0] = "changed"
	*copied.Snapshot.Apps[0].Service.LastExit.Detail.ExitCode = 99
	require.Equal(t, "gguf", ep.Snapshot.Apps[0].Formats[0])
	require.Equal(t, "failed", ep.Snapshot.Apps[0].Service.LastExit.Reason)
	require.Equal(t, []string{"original"}, ep.Snapshot.Apps[0].Service.LastExit.Detail.ErrorTail)
	require.Equal(t, 17, *ep.Snapshot.Apps[0].Service.LastExit.Detail.ExitCode)
}
