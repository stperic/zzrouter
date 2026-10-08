package clientcli

import (
	"encoding/json"
	"fmt"
	"strings"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/ui"
)

// =============================================================================
// PROVIDERS COMMAND
// =============================================================================

// NewProvidersCmd creates the providers command for listing inference providers
func NewProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List inference providers",
		// A mistyped subcommand must not fall through to the listing.
		Args: cobra.NoArgs,
		Long: `List available inference providers across all nodes.

Shows Ollama, vLLM, llama.cpp, MLX and other providers configured in the cluster.

Examples:
  zzrouter providers                   # List all providers
  zzrouter providers --node worker-1    # Filter by node
  zzrouter providers --provider ollama      # Filter by provider type
  zzrouter providers --check-updates   # Also report newer upstream releases
  zzrouter providers resolved llamacpp --model Qwen3.8-27B-Q8_0  # Where each launch value comes from
  zzrouter providers -j                # JSON output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			node, _ := cmd.Flags().GetString("node")
			providerFilter, _ := cmd.Flags().GetString("provider")
			jsonOutput, _ := cmd.Flags().GetBool("json")
			checkUpdates, _ := cmd.Flags().GetBool("check-updates")

			return runProviders(cmd, node, providerFilter, jsonOutput, checkUpdates)
		},
	}

	cmd.Flags().StringP("node", "n", "", "Filter by node name")
	cmd.Flags().StringP("provider", "a", "", "Filter by provider type (ollama, vllm, llamacpp, mlx)")
	cmd.Flags().BoolP("json", "j", false, "Output in JSON format")
	cmd.Flags().Bool("check-updates", false, "Report the newest release each provider's upstream publishes")
	cmd.AddCommand(newProviderAssetsCmd(), newProviderResolvedCmd(), newProviderVariantCmd())

	return cmd
}

func runProviders(cmd *cobra.Command, node, providerFilter string, jsonOutput, checkUpdates bool) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Fetch, parse, and filter providers using shared data layer
	providers, err := shared.FetchProviders(client, node, providerFilter)
	if err != nil {
		return err
	}

	if len(providers) == 0 {
		if format := GetOutputFormat(cmd); format == OutputJSON || jsonOutput {
			fmt.Println("[]")
			return nil
		}
		filterDesc := ""
		if node != "" {
			filterDesc += fmt.Sprintf(" on node '%s'", node)
		}
		if providerFilter != "" {
			filterDesc += fmt.Sprintf(" matching '%s'", providerFilter)
		}
		fmt.Printf("No providers found%s\n", filterDesc)
		return nil
	}

	// An upstream check is opt-in because it is the only part of this
	// command that reaches the network beyond the coordinator.
	if checkUpdates {
		annotateUpstreamVersions(client, providers)
	}

	// Check for JSON output
	if format := GetOutputFormat(cmd); format == OutputJSON || jsonOutput {
		jsonBytes, err := json.MarshalIndent(providers, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to format JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	displayProviders(providers)
	fmt.Printf("\nTotal: %d provider(s)\n", len(providers))
	if checkUpdates {
		fmt.Println("Upgrade with: zzrouter provider upgrade <name> --version <latest>")
	}

	return nil
}

func displayProviders(providers []shared.ProviderInfo) {
	columns := []ui.TableColumn{
		{Header: "PROVIDER", MaxWidth: 20},
		{Header: "VERSION", MaxWidth: 26},
		{Header: "NODE", MaxWidth: 25},
		{Header: "MODE", MaxWidth: 12},
		{Header: "FORMATS", MaxWidth: 30},
	}

	var rows [][]string

	for _, provider := range providers {
		version := providerVersionCell(provider)

		// Prefer the new "kind" discriminator; fall back to legacy
		// "mode" for older coordinators that haven't been updated yet.
		mode := provider.Kind
		if mode == "" {
			mode = provider.Mode
		}
		if mode == "" {
			mode = "external"
		}

		formats := "-"
		if len(provider.Formats) > 0 {
			formats = strings.Join(provider.Formats, ", ")
			if provider.FormatNote != "" {
				formats = fmt.Sprintf("%s(%s)", formats, provider.FormatNote)
			}
			if len(formats) > 30 {
				formats = formats[:27] + "..."
			}
		}

		rows = append(rows, []string{
			provider.Name,
			version,
			provider.Node,
			mode,
			formats,
		})
	}

	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()

	tableData := ui.TableData{
		Columns:    columns,
		Rows:       rows,
		MinColumns: 4,
	}

	if err := renderer.RenderTable(tableData); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

// =============================================================================
// SHARED FIELD HELPERS
// =============================================================================

// annotateUpstreamVersions fills each provider's upstream fields in place.
//
// A provider whose check fails is left as it was rather than aborting the
// listing: the installed versions are the point of the command, and one
// unreachable upstream must not withhold them.
func annotateUpstreamVersions(client *pkgClient.Client, providers []shared.ProviderInfo) {
	// Cache by provider name — the same provider appears once per node, and
	// upstream is a property of the provider, not of the node.
	seen := make(map[string]*pkgClient.ProviderVersionsResponse, len(providers))

	for i := range providers {
		name := providers[i].Name
		report, ok := seen[name]
		if !ok {
			var err error
			report, err = client.GetProviderVersions(name, false)
			if err != nil {
				report = nil
			}
			seen[name] = report
		}
		if report == nil {
			continue
		}

		providers[i].LatestVersion = latestVersionForDisplay(
			providers[i].Version, report.LatestTag, report.Latest)
		providers[i].VersionCheckedAt = report.CheckedAt
		providers[i].VersionStatus = report.PinnedStatus
		for _, n := range report.Nodes {
			if n.Node == providers[i].Node {
				providers[i].VersionStatus = n.Status
				break
			}
		}
	}
}

// latestVersionForDisplay renders the upstream version in the same shape as
// the installed one. The installers disagree on which form they record —
// llama.cpp keeps the raw tag ("b10453"), ollama the normalized version
// ("0.21.2") — so always printing the tag yields "0.21.2 -> v0.32.14", where
// the v reads as part of the change rather than as an artifact.
func latestVersionForDisplay(installed, latestTag, latestVersion string) string {
	if latestTag == "" {
		return latestVersion
	}
	if latestVersion == "" || latestTag == latestVersion {
		return latestTag
	}
	prefix := strings.TrimSuffix(latestTag, latestVersion)
	if prefix != "" && strings.HasPrefix(installed, prefix) {
		return latestTag
	}
	return latestVersion
}

// providerVersionCell renders the installed version, annotated with the
// newest upstream release when one is genuinely newer. Only "newer" earns the
// annotation: "same" needs none, and "unknown" must not imply currency.
func providerVersionCell(provider shared.ProviderInfo) string {
	version := provider.Version
	if version == "" {
		version = "unknown"
	}
	if provider.VersionStatus == "newer" && provider.LatestVersion != "" {
		return version + " -> " + provider.LatestVersion
	}
	if len(version) > 15 {
		version = version[:12] + "..."
	}
	return version
}
