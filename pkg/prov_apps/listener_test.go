package prov_apps

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestLaunchListenerKeepsUnknownAndUsesEffectiveInputs(t *testing.T) {
	for _, test := range []struct {
		name                     string
		declaration              *config.ListenerConfig
		parameters, environment  map[string]string
		scope, host, destination string
	}{
		{name: "unknown", scope: "unknown"},
		{name: "default", declaration: &config.ListenerConfig{DefaultHost: "127.0.0.1"}, scope: "loopback", host: "127.0.0.1", destination: "http://127.0.0.1:8123"},
		{name: "environment", declaration: &config.ListenerConfig{HostEnvironment: "ENGINE_BIND", DefaultHost: "127.0.0.1"}, environment: map[string]string{"ENGINE_BIND": "0.0.0.0"}, scope: "all_interfaces", host: "0.0.0.0", destination: "http://127.0.0.1:8123"},
		{name: "parameter", declaration: &config.ListenerConfig{HostParameter: "host", HostEnvironment: "ENGINE_BIND", DefaultHost: "127.0.0.1"}, parameters: map[string]string{"host": "192.0.2.3"}, environment: map[string]string{"ENGINE_BIND": "0.0.0.0"}, scope: "interface", host: "192.0.2.3", destination: "http://192.0.2.3:8123"},
		{name: "hostname unresolved", declaration: &config.ListenerConfig{DefaultHost: "engine.invalid"}, scope: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := launchListener(config.ServiceConfig{Runtime: &config.AppRuntimeConfig{Listener: test.declaration}}, test.parameters, test.environment, 8123)
			assert.Equal(t, test.scope, view.Scope)
			assert.Equal(t, test.host, view.Host)
			assert.Equal(t, test.destination, view.URL)
			assert.Equal(t, "unknown", view.Authentication, "a listener declaration alone cannot prove native authentication")
		})
	}
}
