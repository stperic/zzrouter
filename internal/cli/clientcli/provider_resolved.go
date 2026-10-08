package clientcli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// resolvedAPI is the part of the client the resolved command uses.
type resolvedAPI interface {
	GetProviderResolved(provider, node, model string) (*pkgClient.ProviderResolved, error)
}

// newProviderResolvedCmd shows what a launch of a model on a node would
// get, and which tier set each value.
func newProviderResolvedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolved <provider>",
		Short: "Show the parameters a launch gets and where each comes from",
		Long: `Resolve a provider's parameter tree for one model on one node, the way
a launch does, and show the tier each value came from:

  default        the provider's own defaults
  model-default  what zzRouter ships for the model's family (PATTERN says
                 which), such as the chat template the family needs
  model          models.<model>, set by an operator (for a variant, its
                 base's cell and then its own)
  node           nodes.<node>
  node-model     nodes.<node>.models.<model>

Later tiers win. To change a model default, set the key at the model tier
with PATCH /zzrouter/v1/providers/<provider>/parameters; "auto" drops an
asset-typed key so the engine uses the model's own file. Rows marked
"request" are request-body defaults: each request for the model gets them
unless it sends the field itself.

Examples:
  zzrouter providers resolved llamacpp --model Qwen3.8-27B-Q8_0
  zzrouter providers resolved llamacpp --model Qwen3.8-27B-Q8_0 --node worker-1
  zzrouter providers resolved mlx --model mlx-community/Qwen3.8-27B-4bit -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := EnsureConnected()
			if err != nil {
				return err
			}
			node, _ := cmd.Flags().GetString("node")
			model, _ := cmd.Flags().GetString("model")
			return runProviderResolved(cmd, client, args[0], node, model)
		},
	}
	cmd.Flags().StringP("node", "n", "", "Node whose tiers take part")
	cmd.Flags().StringP("model", "m", "", "Model whose tiers take part")
	return cmd
}

func runProviderResolved(cmd *cobra.Command, api resolvedAPI, provider, node, model string) error {
	r, err := api.GetProviderResolved(provider, node, model)
	if err != nil {
		return err
	}
	return OutputData(cmd, r, func() { displayResolved(cmd.OutOrStdout(), r) })
}

func displayResolved(out io.Writer, r *pkgClient.ProviderResolved) {
	if r.From != "" {
		fmt.Fprintf(out, "Variant of %s: its weights, its cells, then the variant's own\n\n", r.From)
	}
	if len(r.Parameters)+len(r.Environment)+len(r.Request) == 0 {
		fmt.Fprintln(out, "Nothing set at any tier")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tTIER\tPATTERN\tSHA256")
	row := func(prefix string, values map[string]pkgClient.ResolvedValue) {
		for _, key := range slices.Sorted(maps.Keys(values)) {
			v := values[key]
			fmt.Fprintf(tw, "%s%s\t%v\t%s\t%s\t%s\n", prefix, key, v.Value, v.Tier, dashIfEmpty(v.Pattern), dashIfEmpty(shortDigest(v.SHA256)))
		}
	}
	row("", r.Parameters)
	row("env ", r.Environment)
	row("request ", jsonValues(r.Request))
	_ = tw.Flush()
}

// jsonValues shows request defaults as the JSON the engine receives, so
// an object reads as {"enable_thinking":false}, not as a Go map.
func jsonValues(values map[string]pkgClient.ResolvedValue) map[string]pkgClient.ResolvedValue {
	out := make(map[string]pkgClient.ResolvedValue, len(values))
	for k, v := range values {
		if b, err := json.Marshal(v.Value); err == nil {
			v.Value = string(b)
		}
		out[k] = v
	}
	return out
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
