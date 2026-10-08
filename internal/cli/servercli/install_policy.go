package servercli

import (
	"runtime"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
)

// NewInstallPolicyCmd groups human-run install policy helpers.
func NewInstallPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-policy",
		Short: "Human-owned install policy helpers",
		Long: `An install policy is the root-owned file that lets this node apply
operator overrides of provider install recipes. The node never writes it.`,
	}
	cmd.AddCommand(newInstallPolicyTemplateCmd())
	return cmd
}

func newInstallPolicyTemplateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "template",
		Short: "Print a policy granting exactly this release's shipped recipes",
		Long: `Print an install policy for this platform that approves exactly the
packages, imports, entrypoints and indexes the shipped recipes use.

Install it as root, readable but not writable by the node's account, and point
providers.install_policy_file in node.yaml at it, for example:

  zzrouter-node install-policy template > /tmp/policy.yaml
  sudo install -o root -m 644 /tmp/policy.yaml /etc/zzrouter-install-policy.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := install.MarshalReleasePolicy(runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
}
