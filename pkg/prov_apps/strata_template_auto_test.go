package prov_apps

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// Strata refuses a native pack without --prefill, and its own spelling of
// "let the engine choose" is the value auto: a launch must not drop it.
func TestStrataEngineAutoValuesReachArgv(t *testing.T) {
	schemas, errs := schema.LoadFromFS(templates.AppsFS, "files/providers")
	require.Empty(t, errs)
	declared := schemas["strata"]
	require.NotNil(t, declared)
	params := filterLaunchAuto(declared, "chat", map[string]string{"prefill": "auto", "expert-cache": "auto", "spec": "4"})
	shapes := declared.ForEndpoint("chat")
	for _, key := range []string{"prefill", "expert-cache"} {
		require.Equal(t, "auto", params[key], key)
		require.Equal(t, "auto", shapes[key].Auto, key)
	}
}
