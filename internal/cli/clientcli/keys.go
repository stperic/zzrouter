package clientcli

import (
	"fmt"

	"github.com/spf13/cobra"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// keysAPI is the part of the client the keys commands use.
type keysAPI interface {
	ListKeys() ([]*pkgClient.KeyResponse, error)
	CreateKey(req *pkgClient.CreateKeyRequest) (*pkgClient.CreateKeyResponse, error)
	DeleteKey(id string) error
}

// NewKeysCmd manages virtual keys: the credential each client or agent
// calls zzRouter with, metered and limited on its own.
func NewKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage virtual keys for clients and agents",
		Long: `Manage virtual keys. Give each client or agent a key of its own, never
the admin key, so its usage and spend are metered apart and its limits
apply to it alone. The server validates every value; GET
/zzrouter/v1/keys/schema lists the fields and their allowed values.

Examples:
  zzrouter keys list
  K=$(zzrouter keys create claude-code --name "Claude Code")
  zzrouter keys create ci --rpm 60 --expires 2027-01-01T00:00:00Z -o json
  zzrouter keys rm ci`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List virtual keys",
			Args:  cobra.NoArgs,
			RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, _ []string) error {
				return runKeysList(cmd, api)
			}),
		},
		newKeysCreateCmd(),
		&cobra.Command{
			Use:   "rm <id>",
			Short: "Delete a virtual key; it stops authenticating at once",
			Args:  cobra.ExactArgs(1),
			RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
				return runKeysRm(cmd, api, args[0])
			}),
		},
	)
	return cmd
}

func newKeysCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create a virtual key and print it, once",
		Long: `Create a virtual key. The key itself is printed alone on stdout and is
never shown again, so capture it: K=$(zzrouter keys create <id>).
With -o json the key is printed with its id, name, role and team.`,
		Args: cobra.ExactArgs(1),
		RunE: withClient(func(cmd *cobra.Command, api *pkgClient.Client, args []string) error {
			req, err := keyCreateRequest(cmd, args[0])
			if err != nil {
				return err
			}
			return runKeysCreate(cmd, api, req)
		}),
	}
	f := cmd.Flags()
	f.String("name", "", "Display name (default: the id)")
	f.String("description", "", "Free-form notes")
	f.String("role", "", "user (inference only, the default) or admin")
	f.String("team", "", "Existing team to join (default: a personal team)")
	f.String("team-role", "", "Role in --team: member (default) or owner")
	f.String("expires", "", "Expiry as an RFC 3339 time (default: never)")
	f.Int("rpm", 0, "Requests per minute (0: unlimited)")
	f.Int("tpm", 0, "Tokens per minute (0: unlimited)")
	f.Int("max-parallel", 0, "Concurrent requests (0: unlimited)")
	f.Float64("spend-limit", 0, "USD per --reset-period (0: unlimited)")
	f.String("reset-period", "", "Spend window: daily, weekly or monthly")
	f.Int("default-max-tokens", 0, "max_tokens for requests that send none (0: provider default)")
	return cmd
}

// keyCreateRequest reads the create flags into a request.
func keyCreateRequest(cmd *cobra.Command, id string) (*pkgClient.CreateKeyRequest, error) {
	f := cmd.Flags()
	req := &pkgClient.CreateKeyRequest{ID: id}
	var err error
	for _, s := range []struct {
		flag string
		dst  *string
	}{
		{"name", &req.Name}, {"description", &req.Description}, {"role", &req.Role},
		{"team", &req.TeamID}, {"team-role", &req.TeamRole}, {"reset-period", &req.ResetPeriod},
	} {
		if *s.dst, err = f.GetString(s.flag); err != nil {
			return nil, err
		}
	}
	for _, i := range []struct {
		flag string
		dst  *int
	}{
		{"rpm", &req.RPMLimit}, {"tpm", &req.TPMLimit}, {"max-parallel", &req.MaxParallelRequests},
		{"default-max-tokens", &req.DefaultMaxTokens},
	} {
		if *i.dst, err = f.GetInt(i.flag); err != nil {
			return nil, err
		}
	}
	if req.SpendLimit, err = f.GetFloat64("spend-limit"); err != nil {
		return nil, err
	}
	if req.Name == "" {
		req.Name = id
	}
	expires, err := f.GetString("expires")
	if err != nil {
		return nil, err
	}
	if expires != "" {
		req.ExpiresAt = &expires
	}
	return req, nil
}

func runKeysList(cmd *cobra.Command, api keysAPI) error {
	keys, err := api.ListKeys()
	if err != nil {
		return err
	}
	return OutputData(cmd, keys, func() { displayKeys(keys) })
}

func runKeysCreate(cmd *cobra.Command, api keysAPI, req *pkgClient.CreateKeyRequest) error {
	resp, err := api.CreateKey(req)
	if err != nil {
		return err
	}
	return OutputData(cmd, resp.Data, func() {
		fmt.Fprintln(cmd.OutOrStdout(), resp.Data.RawKey)
		fmt.Fprintf(cmd.ErrOrStderr(), "Created key %s (role %s, team %s). It is shown only this once.\n",
			resp.Data.ID, resp.Data.Role, resp.Data.TeamID)
	})
}

func runKeysRm(cmd *cobra.Command, api keysAPI, id string) error {
	if err := api.DeleteKey(id); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted key %s\n", id)
	return nil
}

func displayKeys(keys []*pkgClient.KeyResponse) {
	if len(keys) == 0 {
		fmt.Println("No keys")
		return
	}
	columns := []ui.TableColumn{
		{Header: "ID", MaxWidth: 32},
		{Header: "NAME", MaxWidth: 32},
		{Header: "ROLE", MaxWidth: 6},
		{Header: "TEAM", MaxWidth: 24},
		{Header: "STATE", MaxWidth: 9},
		{Header: "LAST SEEN", MaxWidth: 25},
	}
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k.ID, k.Name, k.Role, k.TeamID, keyState(k), orDash(k.LastSeenAt)})
	}
	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()
	if err := renderer.RenderTable(ui.TableData{Columns: columns, Rows: rows, MinColumns: 2}); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

// keyState is whether a key authenticates now, and why not.
func keyState(k *pkgClient.KeyResponse) string {
	switch {
	case k.Suspended:
		return "suspended"
	case k.IsExpired:
		return "expired"
	}
	return "active"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
