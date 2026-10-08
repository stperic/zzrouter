package clientcli

import (
	"testing"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func TestNewRunCmd(t *testing.T) {
	cmd := NewRunCmd()

	if cmd == nil {
		t.Fatal("NewRunCmd() returned nil")
	}

	if cmd.Use != "run <model_spec>" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "run <model_spec>")
	}

	if cmd.Short == "" {
		t.Error("cmd.Short should not be empty")
	}

	if cmd.RunE == nil {
		t.Error("cmd.RunE should not be nil")
	}

	// Verify expected flags exist
	flags := cmd.Flags()

	appFlag := flags.Lookup("provider")
	if appFlag == nil {
		t.Error("expected 'app' flag to exist")
	}

	nodeFlag := flags.Lookup("node")
	if nodeFlag == nil {
		t.Error("expected 'node' flag to exist")
	}

	paramFlag := flags.Lookup("param")
	if paramFlag == nil {
		t.Error("expected 'param' flag to exist")
	}

	dryRunFlag := flags.Lookup("dry-run")
	if dryRunFlag == nil {
		t.Error("expected 'dry-run' flag to exist")
	}

	forceFlag := flags.Lookup("force")
	if forceFlag == nil {
		t.Error("expected 'force' flag to exist")
	}

}

func TestParsePositionalModelArg(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		flagValue   string
		expected    string
		expectError bool
	}{
		{
			name:      "positional argument takes precedence",
			args:      []string{"llama3:8b"},
			flagValue: "",
			expected:  "llama3:8b",
		},
		{
			name:      "flag value when no positional",
			args:      []string{},
			flagValue: "gpt-4",
			expected:  "gpt-4",
		},
		{
			name:        "error when both positional and flag provided",
			args:        []string{"mistral"},
			flagValue:   "llama",
			expected:    "",
			expectError: true,
		},
		{
			name:      "empty when neither provided",
			args:      []string{},
			flagValue: "",
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParsePositionalModelArg(tt.args, tt.flagValue)
			if tt.expectError && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("ParsePositionalModelArg(%v, %q) = %q, want %q", tt.args, tt.flagValue, result, tt.expected)
			}
		})
	}
}

type recordingModelLister struct{ repo, app string }

func (r *recordingModelLister) ListModels(f pkgClient.ModelFilter) ([]pkgClient.ModelMetadata, error) {
	r.repo, r.app = f.Registry, f.Provider
	return nil, nil
}

// --provider llamacpp found nothing: it was sent as the registry filter,
// and no weights come from a registry named llamacpp.
func TestRunCandidatesFiltersByProviderNotRegistry(t *testing.T) {
	var lister recordingModelLister
	if _, err := runCandidates(&lister, "worker-1", "llamacpp", "Qwen3.8-27B-Q8_0"); err != nil {
		t.Fatal(err)
	}
	if lister.app != "llamacpp" || lister.repo != "" {
		t.Errorf("provider sent as app=%q repo=%q, want app=llamacpp and no registry filter", lister.app, lister.repo)
	}
}
