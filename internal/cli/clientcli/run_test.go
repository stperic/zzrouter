package clientcli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestNewRunCmd(t *testing.T) {
	cmd := NewRunCmd()

	if cmd == nil {
		t.Fatal("NewRunCmd() returned nil")
	}

	if cmd.Use != "run [model_spec]" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "run [model_spec]")
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
	require.NotNil(t, flags.Lookup("replace"))
	require.NotNil(t, flags.Lookup("endpoint"))
}

func TestRunCommandForceSelectionAndReplace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    []string
		replace  bool
		refuse   bool
		endpoint string
		wantErr  string
		starts   int32
		previews int32
	}{
		{name: "force selects and reuses", flags: []string{"--force"}, starts: 1},
		{name: "endpoint goes on the wire", flags: []string{"--force", "--endpoint", "embeddings"}, endpoint: "embeddings", starts: 1},
		{name: "dry run previews the endpoint", flags: []string{"--force", "--endpoint", "reranking", "--dry-run"}, endpoint: "reranking", previews: 1},
		{name: "replace goes on the wire", flags: []string{"--force", "--replace"}, replace: true, starts: 1},
		{name: "replace leaves selection ambiguous", flags: []string{"--replace"}, wantErr: "multiple models match"},
		{name: "settings refusal suggests replace", flags: []string{"--force"}, refuse: true, wantErr: "Use --replace", starts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			var starts, previews atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case apipath.Health:
					w.WriteHeader(http.StatusOK)
				case apipath.Models:
					_, _ = io.WriteString(w, `{"data":[{"name":"first","node":"worker","assigned_app":"llamacpp"},{"name":"second","node":"worker","assigned_app":"llamacpp"}]}`)
				case apipath.RunsPreview:
					previews.Add(1)
					var preview struct {
						ModelName string `json:"model_name"`
						Endpoint  string `json:"endpoint"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&preview))
					require.Equal(t, "first", preview.ModelName)
					require.Equal(t, tc.endpoint, preview.Endpoint)
					_, _ = io.WriteString(w, `{"data":{"model":"first","provider":"llamacpp","node":"worker"}}`)
				case apipath.Runs:
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"data":[]}`)
						return
					}
					starts.Add(1)
					require.Equal(t, http.MethodPost, r.Method)
					var start pkgClient.StartRunRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&start))
					require.Equal(t, "first", start.Model)
					require.Equal(t, "worker", start.Node)
					require.Equal(t, tc.replace, start.Replace)
					require.Equal(t, tc.endpoint, start.Endpoint)
					if tc.refuse {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"code":"run_settings_differ","detail":"Settings differ"}`)
						return
					}
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"data":{"id":"existing-run","node":"worker","status":"running"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			addr, err := url.Parse(srv.URL)
			require.NoError(t, err)
			port, err := strconv.Atoi(addr.Port())
			require.NoError(t, err)
			cm := config.NewConfigManager("zzrouter")
			require.NoError(t, cm.SaveClientConfig(&config.ClientConfig{Node: config.NodeConnection{Address: addr.Hostname(), Port: port}}))
			cmd := NewRunCmd()
			cmd.SetArgs(append([]string{"*"}, tc.flags...))
			err = cmd.ExecuteContext(t.Context())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.starts, starts.Load())
			require.Equal(t, tc.previews, previews.Load())
		})
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

func TestRunSaveIsUnknownFlag(t *testing.T) {
	cmd := NewRunCmd()
	cmd.SetArgs([]string{"model", "--save"})
	require.ErrorContains(t, cmd.Execute(), "unknown flag: --save")
}
