package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/require"
)

type fakeProviderModelsAPI struct {
	provider, node, category, name string
	ctx                            context.Context
	download                       pkgClient.ProviderModelDownloadRequest
	resolve                        pkgClient.ProviderModelResolveRequest
	failed                         error
}

func (f *fakeProviderModelsAPI) record(ctx context.Context, provider, node, category, name string) {
	f.ctx, f.provider, f.node, f.category, f.name = ctx, provider, node, category, name
}

func (f *fakeProviderModelsAPI) ListProviderModels(ctx context.Context, provider, node, category string) (map[string]any, error) {
	f.record(ctx, provider, node, category, "")
	return map[string]any{"categories": []any{map[string]any{"name": "vae"}}}, f.failed
}

func (f *fakeProviderModelsAPI) DownloadProviderModel(ctx context.Context, provider string, req pkgClient.ProviderModelDownloadRequest) (pkgClient.ProviderModelDownload, error) {
	f.record(ctx, provider, req.Node, req.Category, req.Name)
	f.download = req
	return pkgClient.ProviderModelDownload{JobID: "accepted-job", Node: "returned-owner", Name: "m.safetensors"}, f.failed
}

func (f *fakeProviderModelsAPI) DeleteProviderModel(ctx context.Context, provider, node, category, name string) (map[string]any, error) {
	f.record(ctx, provider, node, category, name)
	return map[string]any{"deleted": true}, f.failed
}

func (f *fakeProviderModelsAPI) ResolveProviderModels(ctx context.Context, provider string, req pkgClient.ProviderModelResolveRequest) (map[string]any, error) {
	f.record(ctx, provider, req.Node, "", "")
	f.resolve = req
	return map[string]any{"requirements": []any{map[string]any{"status": "failed", "reason": "file_exists"}, map[string]any{"status": "downloading", "job_id": "accepted-job"}}}, f.failed
}

func TestProviderModelsCommandsAndRequiredFlags(t *testing.T) {
	group, _, err := NewProvidersCmd().Find([]string{"models"})
	require.NoError(t, err)
	require.Equal(t, "models", group.Name())
	required := map[string][]string{"list": {"node"}, "download": {"node", "category", "source"}, "delete": {"node", "category", "name"}, "resolve": {"node", "workflow"}}
	for verb, flags := range required {
		cmd, _, err := group.Find([]string{verb})
		require.NoError(t, err)
		require.Equal(t, verb+" <provider>", cmd.Use)
		require.Error(t, cmd.Args(cmd, nil))
		require.NoError(t, cmd.Args(cmd, []string{"studio"}))
		for _, flag := range flags {
			fresh := newProviderModelCmd(verb)
			for _, other := range flags {
				if other != flag {
					require.NoError(t, fresh.Flags().Set(other, "value"))
				}
			}
			require.ErrorContains(t, fresh.ValidateRequiredFlags(), flag)
		}
		for _, flag := range flags {
			require.NoError(t, cmd.Flags().Set(flag, "value"))
		}
		require.NoError(t, cmd.ValidateRequiredFlags(), "download name and list category are optional")
	}
}

func TestProviderModelsCommandRequestsAndVisibleResults(t *testing.T) {
	workflow := `{"extra_data":{"extra_pnginfo":{"workflow":{"nodes":[{"properties":{"models":[{"name":"m.safetensors","url":"org/repo#m.safetensors","directory":"vae"}]}}]}}}}`
	file := filepath.Join(t.TempDir(), "workflow.json")
	require.NoError(t, os.WriteFile(file, []byte(workflow), 0600))
	for _, verb := range []string{"list", "download", "delete", "resolve"} {
		t.Run(verb, func(t *testing.T) {
			cmd := newProviderModelCmd(verb)
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			cmd.SetContext(t.Context())
			require.NoError(t, cmd.Flags().Set("node", "requested-node"))
			switch verb {
			case "list":
				require.NoError(t, cmd.Flags().Set("category", "vae"))
			case "download", "delete":
				require.NoError(t, cmd.Flags().Set("category", "vae"))
				require.NoError(t, cmd.Flags().Set("name", "nested/m.safetensors"))
				if verb == "download" {
					require.NoError(t, cmd.Flags().Set("source", "org/repo#m.safetensors"))
				}
			case "resolve":
				require.NoError(t, cmd.Flags().Set("workflow", file))
				require.NoError(t, cmd.Flags().Set("download", "true"))
			}
			api := &fakeProviderModelsAPI{}
			err := runProviderModels(cmd, api, "studio", func(string, string) error { t.Fatal("unexpected watch"); return nil })
			require.NoError(t, err)
			require.Equal(t, "studio", api.provider)
			require.Equal(t, "requested-node", api.node)
			require.Equal(t, cmd.Context(), api.ctx)
			require.True(t, json.Valid(out.Bytes()), "default output preserves the full API data")
			switch verb {
			case "list":
				require.Equal(t, "vae", api.category)
			case "download", "delete":
				require.Equal(t, "nested/m.safetensors", api.name)
				if verb == "download" {
					require.Equal(t, "org/repo#m.safetensors", api.download.Source)
					require.Contains(t, out.String(), "accepted-job")
				}
			case "resolve":
				require.JSONEq(t, workflow, string(api.resolve.Workflow))
				require.True(t, api.resolve.Download)
				require.Contains(t, out.String(), "file_exists")
				require.Contains(t, out.String(), "accepted-job")
			}
		})
	}
}

func TestProviderModelDownloadWatchUsesAcceptedOwnerAndReturnsFailure(t *testing.T) {
	cmd := newProviderModelCmd("download")
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Flags().Set("watch", "true"))
	require.NoError(t, cmd.Flags().Set("node", "requested-node"))
	terminal := errors.New("download job failed")
	api := &fakeProviderModelsAPI{}
	var watched bool
	err := runProviderModels(cmd, api, "studio", func(jobID, node string) error {
		watched = true
		require.Equal(t, "accepted-job", jobID)
		require.Equal(t, "returned-owner", node)
		return terminal
	})
	require.True(t, watched)
	require.ErrorIs(t, err, terminal)
	require.Empty(t, api.download.Name, "server basename selection is preserved")
	api.failed = errors.New("start refused")
	watched = false
	err = runProviderModels(cmd, api, "studio", func(string, string) error { watched = true; return nil })
	require.ErrorIs(t, err, api.failed)
	require.False(t, watched)
}

func TestProviderModelsResolveReadError(t *testing.T) {
	cmd := newProviderModelCmd("resolve")
	api := &fakeProviderModelsAPI{}
	require.NoError(t, cmd.Flags().Set("workflow", filepath.Join(t.TempDir(), "absent")))
	err := runProviderModels(cmd, api, "studio", func(string, string) error { return nil })
	require.ErrorContains(t, err, "read workflow")
	require.Empty(t, api.provider, "read failures do not call the API")
}

var _ providerModelsAPI = (*pkgClient.Client)(nil)
