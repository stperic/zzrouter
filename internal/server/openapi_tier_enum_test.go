package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// The ResolvedValue tier enum must be exactly what /resolved emits, in
// precedence order. It said node_model while the server sent node-model,
// so an agent validating against the spec rejected a real response.
func TestOpenAPIResolvedTierEnum_MatchesTiers(t *testing.T) {
	var want []string
	for tier := pkgConfig.TierDefault; tier <= pkgConfig.TierRequest; tier++ {
		want = append(want, tier.String())
	}
	want = append(want, "feature:vision")
	assert.Equal(t, want, loadOpenAPI(t).Components.Schemas.ResolvedValue.Properties.Tier.Enum)
}
