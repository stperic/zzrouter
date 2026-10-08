package schema_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/schema"
)

// Per-kind fixtures exercise each schema against a minimal valid body.
// Drift-guard: if a required field is added to a per-kind struct in
// pkg/config and the schema isn't updated (or vice versa), a fixture
// will fail here.

func TestValidateOnDemand_MinimalValid(t *testing.T) {
	ok := []byte(`
protocol: openai
runtime:
  port_range: [8000, 8009]
  base_port: 8000
  execution:
    type: cli
    command: vllm
`)
	require.NoError(t, schema.ValidateOnDemand(ok))
}

func TestValidateOnDemand_RejectsMissingPortRange(t *testing.T) {
	bad := []byte(`
protocol: openai
runtime:
  execution:
    type: cli
    command: vllm
`)
	err := schema.ValidateOnDemand(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port_range")
}

func TestValidateOnDemand_RejectsApiExecutionType(t *testing.T) {
	// On-demand can only be cli or python — api belongs to cloud/external.
	bad := []byte(`
protocol: openai
runtime:
  port_range: [8000, 8009]
  base_port: 8000
  execution:
    type: api
`)
	err := schema.ValidateOnDemand(bad)
	require.Error(t, err)
}

func TestValidateExternal_MinimalValid(t *testing.T) {
	ok := []byte(`
protocol: ollama
runtime:
  endpoint: http://localhost:11434
`)
	require.NoError(t, schema.ValidateExternal(ok))
}

func TestValidateExternal_RejectsMissingEndpoint(t *testing.T) {
	bad := []byte(`
protocol: ollama
runtime:
  keep_alive: 5m
`)
	err := schema.ValidateExternal(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}

func TestValidateCloud_MinimalValid(t *testing.T) {
	ok := []byte(`
description: "OpenAI"
runtime:
  endpoint: https://api.openai.com
  api:
    auth_type: bearer
    auth_header: Authorization
    auth_prefix: "Bearer "
    token: "${OPENAI_API_KEY}"
`)
	require.NoError(t, schema.ValidateCloud(ok))
}

func TestValidateCloud_RejectsMissingAuthType(t *testing.T) {
	bad := []byte(`
runtime:
  endpoint: https://api.openai.com
  api:
    auth_header: Authorization
`)
	err := schema.ValidateCloud(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_type")
}

// Cross-kind mutual rejection: each kind's schema must reject a body
// whose *structural shape* belongs to a different kind. This is the
// split's core invariant — putting install_variants (on-demand only)
// under cloud, or a cloud api block under an on-demand runtime, is a
// structural error regardless of whether the offending key happens to
// be caught by additionalProperties:false.
//
// Each fixture below is a valid body of one kind; we assert it validates
// against its own schema and is rejected by every other.
func TestValidate_CrossKindMutualRejection(t *testing.T) {
	type kindCase struct {
		name      string
		body      []byte
		validator func([]byte) error
	}
	onDemand := kindCase{"on-demand",
		[]byte(`
protocol: openai
install_variants:
  - id: linux-amd64
    platforms: [{os: linux, arch: amd64}]
    artifact: vllm.tar.gz
runtime:
  port_range: [8000, 8009]
  base_port: 8000
  execution:
    type: cli
    command: vllm
`), schema.ValidateOnDemand}
	external := kindCase{"external",
		[]byte(`
protocol: ollama
service:
  manager: auto
runtime:
  endpoint: http://localhost:11434
`), schema.ValidateExternal}
	cloud := kindCase{"cloud",
		[]byte(`
runtime:
  endpoint: https://api.openai.com
  api:
    auth_type: bearer
`), schema.ValidateCloud}
	registry := kindCase{"registry",
		[]byte(`
runtime:
  endpoint: https://huggingface.co
`), schema.ValidateRegistry}

	kinds := []kindCase{onDemand, external, cloud, registry}
	for _, self := range kinds {
		self := self
		t.Run(self.name+"_accepts_own_body", func(t *testing.T) {
			t.Parallel()
			require.NoError(t, self.validator(self.body))
		})
		for _, other := range kinds {
			if other.name == self.name {
				continue
			}
			other := other
			t.Run(self.name+"_rejected_by_"+other.name, func(t *testing.T) {
				t.Parallel()
				err := other.validator(self.body)
				require.Errorf(t, err,
					"%s body should not validate against %s schema", self.name, other.name)
			})
		}
	}

	// Spot-check that the rejection reasons mention the shape mismatch,
	// not just "additionalProperties". A cloud body lacks install_variants,
	// but it also lacks protocol — the on-demand schema must reject for
	// *missing required* reasons when the body shape is genuinely foreign.
	err := schema.ValidateOnDemand(cloud.body)
	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "protocol") ||
			strings.Contains(err.Error(), "port_range") ||
			strings.Contains(err.Error(), "required"),
		"expected cross-kind rejection to cite missing required field, got: %v", err)
}

func TestValidateRegistry_MinimalValid(t *testing.T) {
	ok := []byte(`
description: "Hugging Face"
runtime:
  endpoint: https://huggingface.co
`)
	require.NoError(t, schema.ValidateRegistry(ok))
}

func TestValidateRegistry_RejectsApiBlock(t *testing.T) {
	// Registries don't have api auth — that field is cloud-only.
	bad := []byte(`
runtime:
  endpoint: https://huggingface.co
  api:
    auth_type: bearer
`)
	err := schema.ValidateRegistry(bad)
	require.Error(t, err)
}
