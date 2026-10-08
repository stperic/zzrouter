package clientcli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// variantAPI is the part of the client the variant commands use.
type variantAPI interface {
	UpdateModelCell(provider, model string, u pkgClient.ModelCellUpdate, restart bool) (*pkgClient.ProviderParametersWrite, error)
	DeleteModelCell(provider, model string) error
	GetProviderResolved(provider, node, model string) (*pkgClient.ProviderResolved, error)
}

// newProviderVariantCmd defines and removes variants: a model name of its
// own over another model's weights, with its own launch settings and
// request defaults.
func newProviderVariantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "variant",
		Short: "Define named variants of a model: same weights, other settings",
		Long: `A variant is a model name of its own over another model's weights, like an
Ollama Modelfile (FROM + PARAMETER) or a LiteLLM model_list alias. Clients
select it by name; /v1/models lists it wherever its base's weights are.

  --param    launch-time: becomes the engine's flag, e.g. ctx-size or
             chat-template-file. A variant whose launch differs from its
             base's runs in a process of its own.
  --request  request-time: a body field each request gets unless it sends
             it, e.g. temperature or chat_template_kwargs. A variant with
             only these shares its base's process.

The name is any literal of letters, digits and . _ + - ; by convention
<base>+<variant>.

Examples:
  zzrouter providers variant set llamacpp Qwen3.8-27B-Q8_0+agent --from Qwen3.8-27B-Q8_0 \
      --param ctx-size=131072 --request 'chat_template_kwargs={"enable_thinking":false}'
  zzrouter providers variant set llamacpp Qwen3.8-27B-Q8_0+chat --from Qwen3.8-27B-Q8_0 \
      --request temperature=0.7
  zzrouter providers variant set llamacpp Qwen3.8-27B-Q8_0+chat --unset-request temperature
  zzrouter providers variant rm llamacpp Qwen3.8-27B-Q8_0+chat`,
	}
	cmd.AddCommand(newVariantSetCmd(), &cobra.Command{
		Use:   "rm <provider> <name>",
		Short: "Remove a variant",
		Args:  cobra.ExactArgs(2),
		RunE: withClient(func(cmd *cobra.Command, c *pkgClient.Client, args []string) error {
			return runVariantRm(cmd, c, args[0], args[1])
		}),
	})
	return cmd
}

func newVariantSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <provider> <name>",
		Short: "Create or update a variant",
		Args:  cobra.ExactArgs(2),
		RunE: withClient(func(cmd *cobra.Command, c *pkgClient.Client, args []string) error {
			u, err := variantUpdateFromFlags(cmd)
			if err != nil {
				return err
			}
			restart, _ := cmd.Flags().GetBool("restart")
			return runVariantSet(cmd, c, args[0], args[1], u, restart)
		}),
	}
	cmd.Flags().String("from", "", "Model whose weights the variant runs (required on create)")
	cmd.Flags().StringArray("param", nil, "Launch parameter key=value, typed by the provider's schema")
	cmd.Flags().StringArray("request", nil, "Request default key=<JSON value>; a value that is not JSON is a string")
	cmd.Flags().StringArray("unset-param", nil, "Launch parameter to delete")
	cmd.Flags().StringArray("unset-request", nil, "Request default to delete")
	cmd.Flags().Bool("restart", false, "Restart the running models the change leaves stale")
	return cmd
}

// variantUpdateFromFlags reads variant set's flags into the update they
// describe. A key both set and unset is refused rather than one silently
// winning.
func variantUpdateFromFlags(cmd *cobra.Command) (pkgClient.ModelCellUpdate, error) {
	var u pkgClient.ModelCellUpdate
	u.From, _ = cmd.Flags().GetString("from")
	u.Unset, _ = cmd.Flags().GetStringArray("unset-param")
	u.UnsetRequest, _ = cmd.Flags().GetStringArray("unset-request")
	params, _ := cmd.Flags().GetStringArray("param")
	request, _ := cmd.Flags().GetStringArray("request")
	if len(params) > 0 {
		u.Parameters = make(map[string]string, len(params))
	}
	for _, item := range params {
		k, v, err := splitKeyValue("--param", item)
		if err != nil {
			return u, err
		}
		u.Parameters[k] = v
	}
	var err error
	if u.Request, err = parseRequest(request); err != nil {
		return u, err
	}
	for _, k := range u.Unset {
		if _, set := u.Parameters[k]; set {
			return u, fmt.Errorf("--param and --unset-param both name %q", k)
		}
	}
	for _, k := range u.UnsetRequest {
		if _, set := u.Request[k]; set {
			return u, fmt.Errorf("--request and --unset-request both name %q", k)
		}
	}
	return u, nil
}

func runVariantSet(cmd *cobra.Command, api variantAPI, provider, name string, u pkgClient.ModelCellUpdate, restart bool) error {
	if u.From == "" && len(u.Parameters)+len(u.Unset)+len(u.Request)+len(u.UnsetRequest) == 0 {
		return fmt.Errorf("nothing to set: give --from, --param, --request or an --unset flag")
	}
	res, err := api.UpdateModelCell(provider, name, u, restart)
	if err != nil {
		return err
	}
	return OutputData(cmd, res, func() {
		out := cmd.OutOrStdout()
		displayResolved(out, &res.ProviderResolved)
		displayRunsReport(out, res.Runs)
	})
}

// parseRequest reads key=value request defaults. The value is JSON when
// it parses as JSON (0.7, false, {"enable_thinking":false}) and a string
// otherwise.
func parseRequest(items []string) (map[string]any, error) {
	out := make(map[string]any, len(items))
	for _, item := range items {
		key, value, err := splitKeyValue("--request", item)
		if err != nil {
			return nil, err
		}
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			out[key] = decoded
		} else {
			out[key] = value
		}
	}
	return out, nil
}

// runVariantRm removes a variant. It deletes the whole models.<name>
// entry, so a name that is not a variant is refused rather than having
// its operator settings wiped.
func runVariantRm(cmd *cobra.Command, api variantAPI, provider, name string) error {
	resolved, err := api.GetProviderResolved(provider, "", name)
	if err != nil {
		return err
	}
	if resolved.From == "" {
		return fmt.Errorf("%s is not a variant of %s; its settings are edited with providers parameters", name, provider)
	}
	if err := api.DeleteModelCell(provider, name); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s\n", name, provider)
	return nil
}
