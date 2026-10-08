package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config"
)

// TestOllamaVersionEndpoint_FromServiceConfig exercises the mapping
// ProbeOllama depends on: the first http-method rule's target is the
// endpoint.
func TestOllamaVersionEndpoint_FromServiceConfig(t *testing.T) {
	svc := &config.ServiceConfig{
		Discovery: &config.AppDiscovery{
			Detection: []config.AppDetectionRule{
				{Method: "http", Target: "http://localhost:11434/api/version"},
			},
		},
	}
	assert.Equal(t, "http://localhost:11434/api/version", OllamaVersionEndpoint(svc))
}

func TestOllamaVersionEndpoint_NilSafe(t *testing.T) {
	assert.Equal(t, "", OllamaVersionEndpoint(nil))
	assert.Equal(t, "", OllamaVersionEndpoint(&config.ServiceConfig{}))
	assert.Equal(t, "", OllamaVersionEndpoint(&config.ServiceConfig{Discovery: &config.AppDiscovery{}}))
}

// TestOllamaYAMLCarriesVersionEndpoint guards the load-bearing invariant
// in pkg/config/templates/files/providers/external/ollama.yaml: the
// discovery block MUST declare an http-method detection rule, because
// ProbeOllama reads its target to decide where to probe. A dead-YAML
// cleanup commit that strips discovery.detection from external/ollama.yaml
// would break boot-time Ollama auto-enable silently without this check.
func TestOllamaYAMLCarriesVersionEndpoint(t *testing.T) {
	path := locateOllamaTemplate(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read template %s", path)

	// Decode the YAML as a raw map first so we don't pin this test to
	// the full ServiceConfig shape — we only care about the detection
	// target, which is what the runtime consumes.
	var body struct {
		Discovery struct {
			Detection []struct {
				Method string `yaml:"method"`
				Target string `yaml:"target"`
			} `yaml:"detection"`
		} `yaml:"discovery"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &body), "parse template %s", path)

	found := ""
	for _, r := range body.Discovery.Detection {
		if strings.EqualFold(r.Method, "http") && r.Target != "" {
			found = r.Target
			break
		}
	}
	require.NotEmpty(t, found, "external/ollama.yaml must declare an http detection rule with a non-empty target")
	assert.Contains(t, found, "/api/version", "expected Ollama version endpoint path")
}

// locateOllamaTemplate returns the path to the bundled ollama provider
// template. The test file is at pkg/prov_apps/detect/; the template lives
// at pkg/config/templates/files/providers/external/ollama.yaml, three
// parents up from this file.
func locateOllamaTemplate(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	// .../pkg/prov_apps/detect/ollama_test.go
	detectDir := filepath.Dir(thisFile)
	repoPkg := filepath.Join(detectDir, "..", "..", "..", "pkg")
	return filepath.Join(repoPkg, "config", "templates", "files", "providers", "external", "ollama", "config.yaml")
}
