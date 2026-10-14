package clientcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

type providerModelsAPI interface {
	ListProviderModels(context.Context, string, string, string) (map[string]any, error)
	DownloadProviderModel(context.Context, string, pkgClient.ProviderModelDownloadRequest) (pkgClient.ProviderModelDownload, error)
	DeleteProviderModel(context.Context, string, string, string, string) (map[string]any, error)
	ResolveProviderModels(context.Context, string, pkgClient.ProviderModelResolveRequest) (map[string]any, error)
}

func newProviderModelsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "Manage provider model files on a node", Args: cobra.NoArgs}
	for _, verb := range []string{"list", "download", "delete", "resolve"} {
		cmd.AddCommand(newProviderModelCmd(verb))
	}
	return cmd
}

func newProviderModelCmd(verb string) *cobra.Command {
	cmd := &cobra.Command{
		Use: verb + " <provider>", Short: verb + " provider model files", Args: cobra.ExactArgs(1),
		RunE: withClient(func(cmd *cobra.Command, client *pkgClient.Client, args []string) error {
			return runProviderModels(cmd, client, args[0], func(jobID, node string) error {
				return RenderJobProgress(cmd.Context(), client, jobID, node, "Downloading")
			})
		}),
	}
	cmd.Flags().StringP("node", "n", "", "Named node owning the files (required)")
	_ = cmd.MarkFlagRequired("node")
	if verb != "resolve" {
		cmd.Flags().String("category", "", "Declared model category")
		if verb != "list" {
			_ = cmd.MarkFlagRequired("category")
			cmd.Flags().String("name", "", "Relative file name (download defaults to source basename)")
		}
	}
	switch verb {
	case "download":
		cmd.Flags().String("source", "", "Hugging Face repo#file or resolve URL (required)")
		_ = cmd.MarkFlagRequired("source")
		cmd.Flags().Bool("watch", false, "Watch the accepted download job")
	case "delete":
		_ = cmd.MarkFlagRequired("name")
	case "resolve":
		cmd.Flags().String("workflow", "", "UI workflow or submission envelope JSON file (required)")
		_ = cmd.MarkFlagRequired("workflow")
		cmd.Flags().Bool("download", false, "Start downloads for missing resolvable requirements")
	}
	return cmd
}

func runProviderModels(cmd *cobra.Command, api providerModelsAPI, provider string, watch func(jobID, node string) error) error {
	node, _ := cmd.Flags().GetString("node")
	category, _ := cmd.Flags().GetString("category")
	name, _ := cmd.Flags().GetString("name")
	var result any
	var err error
	switch cmd.Name() {
	case "list":
		result, err = api.ListProviderModels(cmd.Context(), provider, node, category)
	case "download":
		source, _ := cmd.Flags().GetString("source")
		var download pkgClient.ProviderModelDownload
		download, err = api.DownloadProviderModel(cmd.Context(), provider, pkgClient.ProviderModelDownloadRequest{Node: node, Category: category, Name: name, Source: source})
		if err != nil {
			return err
		}
		result = download
		if err := outputProviderModels(cmd, result); err != nil {
			return err
		}
		if follow, _ := cmd.Flags().GetBool("watch"); follow {
			return watch(download.JobID, download.Node)
		}
		return nil
	case "delete":
		result, err = api.DeleteProviderModel(cmd.Context(), provider, node, category, name)
	case "resolve":
		path, _ := cmd.Flags().GetString("workflow")
		workflow, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read workflow: %w", readErr)
		}
		start, _ := cmd.Flags().GetBool("download")
		result, err = api.ResolveProviderModels(cmd.Context(), provider, pkgClient.ProviderModelResolveRequest{Node: node, Workflow: workflow, Download: start})
	}
	if err != nil {
		return err
	}
	return outputProviderModels(cmd, result)
}

func outputProviderModels(cmd *cobra.Command, result any) error {
	// Keep per-file statuses, reasons and job IDs visible in the default output.
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return OutputData(cmd, result, func() { fmt.Fprintln(cmd.OutOrStdout(), string(data)) })
}
