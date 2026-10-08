package clientcli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// assetsAPI is the part of the client the assets commands use.
type assetsAPI interface {
	ListProviderAssets(provider string) ([]pkgClient.ProviderAsset, error)
	GetProviderAsset(provider, name string) ([]byte, error)
	PutProviderAsset(provider, name string, data []byte, restart bool) (pkgClient.ProviderAssetWrite, error)
	DeleteProviderAsset(provider, name string) error
}

// newProviderAssetsCmd manages the files a provider's asset-typed
// parameters name.
func newProviderAssetsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assets",
		Short: "Manage files a provider's asset parameters name",
		Long: `Manage a provider's asset files: small files, such as a chat template,
that a parameter of type asset takes by name. The coordinator holds them
and every worker gets a copy; a launch resolves the name to the file on
the node that runs it.

Examples:
  zzrouter providers assets list llamacpp
  zzrouter providers assets put llamacpp my-template.jinja ./my-template.jinja
  zzrouter providers assets get llamacpp my-template.jinja --file ./copy.jinja
  zzrouter providers assets rm llamacpp my-template.jinja`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list <provider>",
			Short: "List a provider's assets and where they are used",
			Args:  cobra.ExactArgs(1),
			RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
				return runAssetsList(cmd, api, args[0])
			}),
		},
		newAssetsGetCmd(),
		newAssetsPutCmd(),
		&cobra.Command{
			Use:   "rm <provider> <name>",
			Short: "Delete an asset no parameter names",
			Args:  cobra.ExactArgs(2),
			RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
				return runAssetsRm(cmd, api, args[0], args[1])
			}),
		},
	)
	return cmd
}

func newAssetsGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <provider> <name>",
		Short: "Write an asset's bytes to stdout or a file",
		Args:  cobra.ExactArgs(2),
		RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			return runAssetsGet(cmd, api, args[0], args[1], file)
		}),
	}
	// Not -o: that is the global output-format flag.
	cmd.Flags().String("file", "", "Write to this file instead of stdout")
	return cmd
}

func newAssetsPutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "put <provider> <name> <file|->",
		Short: "Create or replace an asset from a file, or stdin with -",
		Args:  cobra.ExactArgs(3),
		RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
			restart, _ := cmd.Flags().GetBool("restart")
			return runAssetsPut(cmd, api, args[0], args[1], args[2], restart)
		}),
	}
	cmd.Flags().Bool("restart", false, "Restart the running models the new content leaves stale")
	return cmd
}

func runAssetsList(cmd *cobra.Command, api assetsAPI, provider string) error {
	list, err := api.ListProviderAssets(provider)
	if err != nil {
		return err
	}
	return OutputData(cmd, list, func() { displayAssets(list) })
}

func runAssetsGet(cmd *cobra.Command, api assetsAPI, provider, name, file string) error {
	data, err := api.GetProviderAsset(provider, name)
	if err != nil {
		return err
	}
	if file != "" {
		return os.WriteFile(file, data, assetFileMode)
	}
	_, err = cmd.OutOrStdout().Write(data)
	return err
}

func runAssetsPut(cmd *cobra.Command, api assetsAPI, provider, name, src string, restart bool) error {
	data, err := readAssetSource(cmd.InOrStdin(), src)
	if err != nil {
		return err
	}
	asset, err := api.PutProviderAsset(provider, name, data, restart)
	if err != nil {
		return err
	}
	return OutputData(cmd, asset, func() {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Stored %s for %s (%s, sha256 %s)\n",
			asset.Name, provider, shared.FormatSize(asset.Size), asset.SHA256)
		displayRunsReport(out, asset.Runs)
	})
}

func runAssetsRm(cmd *cobra.Command, api assetsAPI, provider, name string) error {
	if err := api.DeleteProviderAsset(provider, name); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s from %s\n", name, provider)
	return nil
}

// assetFileMode is the mode of a file `assets get --file` writes.
const assetFileMode = 0o644

// readAssetSource reads the file to upload; "-" is stdin.
func readAssetSource(stdin io.Reader, src string) ([]byte, error) {
	if src == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(src) //nolint:gosec // the operator names the file to upload
}

// displayAssets prints to stdout, where the table renderer writes.
func displayAssets(list []pkgClient.ProviderAsset) {
	if len(list) == 0 {
		fmt.Println("No assets")
		return
	}
	columns := []ui.TableColumn{
		{Header: "NAME", MaxWidth: 40},
		{Header: "SIZE", MaxWidth: 10},
		{Header: "SHA256", MaxWidth: 14},
		{Header: "SHIPPED", MaxWidth: 7},
		{Header: "USED BY", MaxWidth: 60},
	}
	rows := make([][]string, 0, len(list))
	for _, a := range list {
		used := "-"
		if len(a.ReferencedBy) > 0 {
			used = strings.Join(a.ReferencedBy, ", ")
		}
		shipped := "no"
		if a.Shipped {
			shipped = "yes"
		}
		rows = append(rows, []string{a.Name, shared.FormatSize(a.Size), shortDigest(a.SHA256), shipped, used})
	}
	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()
	if err := renderer.RenderTable(ui.TableData{Columns: columns, Rows: rows, MinColumns: 2}); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

// shortDigestLen is enough of a sha256 to tell assets apart at a glance.
const shortDigestLen = 12

func shortDigest(d string) string {
	if len(d) <= shortDigestLen {
		return d
	}
	return d[:shortDigestLen]
}

// displayRunsReport says what a provider write means for the running
// models: one line per run it lists, then the restart job if one started.
func displayRunsReport(out io.Writer, r *pkgClient.RunsReport) {
	if r == nil {
		return
	}
	if r.Error != "" {
		fmt.Fprintf(out, "Running models could not be checked: %s\n", r.Error)
		return
	}
	line := func(state string, run pkgClient.RunRef, detail []string, reason string) {
		why := strings.Join(detail, ", ")
		if reason != "" {
			why = reason
		}
		fmt.Fprintf(out, "  %-10s %s on %s (%s): %s\n", state, run.Model, run.Node, run.ID, why)
	}
	for _, run := range r.Stale {
		line("stale", run, run.Changed, "")
	}
	for _, run := range r.Overridden {
		line("overridden", run, run.Overridden, "")
	}
	for _, run := range r.Unknown {
		line("unknown", run, nil, run.Reason)
	}
	switch {
	case r.RestartJobID != "":
		fmt.Fprintf(out, "Restarting %d stale run(s) in job %s\n", len(r.Stale), r.RestartJobID)
	case len(r.Stale) > 0:
		fmt.Fprintln(out, "Run again with --restart to restart the stale runs")
	case len(r.Overridden)+len(r.Unknown) == 0:
		fmt.Fprintln(out, "No running model is affected")
	}
}
