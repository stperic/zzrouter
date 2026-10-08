package control

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// stubGroups is a GroupReader over a fixed map. Only Get is exercised.
type stubGroups map[string]*modelgroup.ModelGroup

func (g stubGroups) Get(name string) *modelgroup.ModelGroup { return g[name] }

func (g stubGroups) Names() []string {
	names := make([]string, 0, len(g))
	for n := range g {
		names = append(names, n)
	}
	return names
}

// bareName is the test's stand-in for the server's model-identifier
// parser: it drops an @node decoration the way splitModelRef does.
func bareName(model string) string {
	if at := strings.LastIndex(model, "@"); at > 0 {
		return model[:at]
	}
	return model
}

func TestAuthorizer_Allowed(t *testing.T) {
	t.Parallel()

	groups := stubGroups{
		"route-qwen": {Replicas: []modelgroup.Replica{
			{Name: "r1", Model: "qwen2.5:0.5b@macbook-pro", App: "ollama"},
		}},
	}

	tests := []struct {
		name       string
		identity   ModelIdentity
		requested  string
		teamModels []string
		want       bool
	}{
		{
			name:       "empty list is a wildcard",
			requested:  "anything",
			teamModels: nil,
			want:       true,
		},
		{
			name:       "exact match",
			requested:  "qwen2.5:0.5b",
			teamModels: []string{"qwen2.5:0.5b"},
			want:       true,
		},
		{
			name:       "no match",
			requested:  "smollm:135m",
			teamModels: []string{"qwen2.5:0.5b"},
			want:       false,
		},
		{
			// The reported defect: the entry is an id copied straight out
			// of /v1/models, the request is what the dispatcher strips it
			// down to. Without an identity func the two never meet.
			name:       "catalog id in the allow list matches the stripped request",
			identity:   bareName,
			requested:  "qwen2.5:0.5b",
			teamModels: []string{"qwen2.5:0.5b@macbook-pro"},
			want:       true,
		},
		{
			name:       "bare entry still matches a decorated request",
			identity:   bareName,
			requested:  "qwen2.5:0.5b@worker-1",
			teamModels: []string{"qwen2.5:0.5b"},
			want:       true,
		},
		{
			name:       "normalizing does not widen across models",
			identity:   bareName,
			requested:  "smollm:135m",
			teamModels: []string{"qwen2.5:0.5b@macbook-pro"},
			want:       false,
		},
		{
			name:       "group alias expands to its replicas",
			identity:   bareName,
			requested:  "qwen2.5:0.5b",
			teamModels: []string{"route-qwen"},
			want:       true,
		},
		{
			name:       "group alias is matched literally too",
			identity:   bareName,
			requested:  "route-qwen",
			teamModels: []string{"route-qwen"},
			want:       true,
		},
		{
			name:       "nil identity compares verbatim",
			requested:  "qwen2.5:0.5b",
			teamModels: []string{"qwen2.5:0.5b@macbook-pro"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := NewAuthorizer(groups, tt.identity)
			assert.Equal(t, tt.want, a.Allowed(tt.requested, tt.teamModels))
		})
	}
}
