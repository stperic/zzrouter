package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDataSubdirectoryValidation(t *testing.T) {
	for name, edit := range map[string]func(*OnDemandProvider){
		"valid":        func(p *OnDemandProvider) {},
		"missing data": func(p *OnDemandProvider) { p.Runtime.DataDir = ""; p.Runtime.Execution.Args = nil },
		"wrong scope":  func(p *OnDemandProvider) { p.RunScope = "model"; p.Protocol = ProtocolOpenAI },
		"duplicate":    func(p *OnDemandProvider) { p.Runtime.DataSubdirs = []string{"custom_nodes", "custom_nodes"} },
		"nested":       func(p *OnDemandProvider) { p.Runtime.DataSubdirs = []string{"a/b"} },
		"escape":       func(p *OnDemandProvider) { p.Runtime.DataSubdirs = []string{"../a"} },
		"uppercase":    func(p *OnDemandProvider) { p.Runtime.DataSubdirs = []string{"CUSTOM"} },
		"long":         func(p *OnDemandProvider) { p.Runtime.DataSubdirs = []string{strings.Repeat("a", 129)} },
		"many":         func(p *OnDemandProvider) { p.Runtime.DataSubdirs = make([]string, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			p := modelStoreProvider()
			p.Runtime.ModelStore = nil
			edit(p)
			err := p.Validate()
			raw, marshalErr := yaml.Marshal(p)
			require.NoError(t, marshalErr)
			_, decodeErr := DecodeProviderBytes(KindOnDemand, p.Name, raw)
			if name == "valid" {
				require.NoError(t, err)
				require.NoError(t, decodeErr)
			} else {
				require.ErrorContains(t, err, "data_subdirs")
				require.Error(t, decodeErr)
			}
		})
	}
}

func TestDataSubdirectoriesCannotDisappearAcrossKindConversion(t *testing.T) {
	p := modelStoreProvider()
	p.Runtime.DataDir = ""
	p.Runtime.ModelStore = nil
	p.Runtime.Execution.Args = nil
	for _, mode := range []string{"external", "service", "cloud", "registry"} {
		t.Run(mode, func(t *testing.T) {
			sc := providerToServiceConfig(p)
			sc.Mode = mode
			_, err := serviceConfigToProvider(p.Name, sc)
			require.ErrorContains(t, err, "on-demand ownership")
		})
	}
}
