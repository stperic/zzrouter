package clientcli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// NewConsoleCmd opens the configured coordinator's web console.
func NewConsoleCmd() *cobra.Command {
	return newConsoleCmd(func(ctx context.Context) (string, error) {
		host, err := connectedNodeConfig()
		if err != nil {
			return "", err
		}
		return pkgClient.NewClient(*host).ConsoleURL(ctx, pkgClient.ConsoleTarget{})
	}, shared.OpenFile, shared.BrowserAvailable)
}

func newConsoleCmd(discover func(context.Context) (string, error), open func(string) error, browserAvailable func() bool) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use: "console", Short: "Open the coordinator's web console",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			consoleURL, err := discover(cmd.Context())
			if err != nil {
				return err
			}
			result := struct {
				URL string `json:"url"`
			}{URL: consoleURL}
			if GetOutputFormat(cmd) != OutputTable {
				return OutputData(cmd, result, nil)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), consoleURL); err != nil {
				return err
			}
			if noOpen {
				return nil
			}
			if !browserAvailable() {
				cmd.PrintErrln("Open this URL in a browser on your computer. Automatic opening is unavailable in this terminal.")
				return nil
			}
			if err := open(consoleURL); err != nil {
				return fmt.Errorf("could not open the browser; open the printed URL manually: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "print", false, "Print the console URL without opening a browser")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Alias for --print")
	return cmd
}
