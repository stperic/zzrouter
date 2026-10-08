package clientcli

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
)

func TestExtractPort(t *testing.T) {
	tests := []struct {
		name     string
		cmdline  string
		expected int
	}{
		{
			name:     "explicit port flag with equals",
			cmdline:  "zzrouter-node start --port=8080",
			expected: 8080,
		},
		{
			name:     "explicit port flag with space",
			cmdline:  "zzrouter-node start --port 9000",
			expected: 9000,
		},
		{
			name:     "short port flag",
			cmdline:  "zzrouter-node start -p 3000",
			expected: 3000,
		},
		{
			name:     "localhost with port",
			cmdline:  "zzrouter-node --bind localhost:5000",
			expected: 5000,
		},
		{
			name:     "0.0.0.0 with port",
			cmdline:  "zzrouter-node --bind 0.0.0.0:8888",
			expected: 8888,
		},
		{
			name:     "127.0.0.1 with port",
			cmdline:  "zzrouter-node --listen 127.0.0.1:7777",
			expected: 7777,
		},
		{
			name:     "default port for zzrouter-node",
			cmdline:  "zzrouter-node start",
			expected: constants.DefaultZZROUTERPort,
		},
		{
			// "zzrouter host" was retired; no longer recognized as a
			// fallback trigger for the default port.
			name:     "no default port for retired zzrouter host subcommand",
			cmdline:  "zzrouter host start",
			expected: 0,
		},
		{
			name:     "no port for unrecognized command",
			cmdline:  "some-other-server start",
			expected: 0,
		},
		{
			name:     "port at end of string",
			cmdline:  "server listening on :9090",
			expected: 9090,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPort(tt.cmdline)
			if result != tt.expected {
				t.Errorf("extractPort(%q) = %d, want %d", tt.cmdline, result, tt.expected)
			}
		})
	}
}

func TestParseURL(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		expectedNode string
		expectedPort int
	}{
		{
			name:         "http with explicit port",
			url:          "http://localhost:9090",
			expectedNode: "localhost",
			expectedPort: 9090,
		},
		{
			name:         "https with explicit port",
			url:          "https://example.com:8443",
			expectedNode: "example.com",
			expectedPort: 8443,
		},
		{
			name:         "ip address with port",
			url:          "http://192.168.1.100:3000",
			expectedNode: "192.168.1.100",
			expectedPort: 3000,
		},
		{
			name:         "host without port defaults",
			url:          "http://myserver",
			expectedNode: "myserver",
			expectedPort: constants.DefaultZZROUTERPort,
		},
		{
			name:         "host without protocol",
			url:          "localhost:8080",
			expectedNode: "localhost",
			expectedPort: 8080,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := parseURL(tt.url)
			if host != tt.expectedNode {
				t.Errorf("parseURL(%q) host = %q, want %q", tt.url, host, tt.expectedNode)
			}
			if port != tt.expectedPort {
				t.Errorf("parseURL(%q) port = %d, want %d", tt.url, port, tt.expectedPort)
			}
		})
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected string
	}{
		{
			name:     "short key fully masked",
			key:      "abc123",
			expected: "******",
		},
		{
			name:     "medium key partially masked",
			key:      "abcd1234efgh",
			expected: "abcd****efgh",
		},
		{
			name:     "long key shows first and last 4",
			key:      "abcdefghijklmnopqrst",
			expected: "abcd************qrst",
		},
		{
			name:     "empty key",
			key:      "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskKey(tt.key)
			if result != tt.expected {
				t.Errorf("maskKey(%q) = %q, want %q", tt.key, result, tt.expected)
			}
		})
	}
}

func TestNewConnectCmd(t *testing.T) {
	cmd := NewConnectCmd()

	if cmd == nil {
		t.Fatal("NewConnectCmd() returned nil")
	}

	if cmd.Use != "connect [url]" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "connect [url]")
	}

	if cmd.Short == "" {
		t.Error("cmd.Short should not be empty")
	}

	if cmd.RunE == nil {
		t.Error("cmd.RunE should not be nil")
	}
}

func TestNewDisconnectCmd(t *testing.T) {
	cmd := NewDisconnectCmd()

	if cmd == nil {
		t.Fatal("NewDisconnectCmd() returned nil")
	}

	if cmd.Use != "disconnect" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "disconnect")
	}

	if cmd.Short == "" {
		t.Error("cmd.Short should not be empty")
	}

	if cmd.RunE == nil {
		t.Error("cmd.RunE should not be nil")
	}
}

func TestNewStatusCmd(t *testing.T) {
	cmd := NewStatusCmd()

	if cmd == nil {
		t.Fatal("NewStatusCmd() returned nil")
	}

	if cmd.Use != "status" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "status")
	}
}

func TestNewNodesCmd(t *testing.T) {
	cmd := NewNodesCmd()

	if cmd == nil {
		t.Fatal("NewNodesCmd() returned nil")
	}

	if cmd.Use != "nodes" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "nodes")
	}
}

func TestNewProvidersCmd(t *testing.T) {
	cmd := NewProvidersCmd()

	if cmd == nil {
		t.Fatal("NewProvidersCmd() returned nil")
	}

	if cmd.Use != "providers" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "providers")
	}
}
