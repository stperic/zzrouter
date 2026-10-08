package clientcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewConfigCmd creates the config command for displaying configuration information
func NewConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage zzRouter client configuration",
		Long: `Display or edit client-side configuration including hosts and preferences.

Examples:
  zzrouter config              # Show client config
  zzrouter config --edit       # Edit client config
  zzrouter config reset        # Reset to defaults (with confirmation)
  zzrouter config reset --yes  # Reset to defaults (no prompt)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			edit, _ := cmd.Flags().GetBool("edit")
			if edit {
				return runConfigClientEdit()
			}
			return runConfigClient()
		},
	}

	cmd.Flags().BoolP("edit", "e", false, "Edit configuration file in your preferred editor")

	// Add subcommands
	cmd.AddCommand(newConfigResetCmd())
	cmd.AddCommand(newConfigThemeCmd())

	return cmd
}

// newConfigResetCmd creates the reset subcommand for client config
func newConfigResetCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset client configuration to defaults",
		Long: `Reset client configuration (cli.yaml) to default values.

This overwrites the existing client configuration with built-in defaults.

Examples:
  zzrouter config reset        # Prompts for confirmation
  zzrouter config reset --yes  # Skip confirmation prompt`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigClientReset(yes)
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// runConfigClientReset resets client configuration to defaults
func runConfigClientReset(yes bool) error {
	if !yes {
		if !confirmYN("Reset client config to defaults? [y/N]: ") {
			fmt.Println("Reset cancelled.")
			return nil
		}
	}

	cm := pkgConfig.NewConfigManager("zzrouter")
	configDir := cm.GetClientConfigDir()
	if err := cm.EnsureDir(configDir); err != nil {
		return fmt.Errorf("failed to create client config dir: %w", err)
	}

	configFile := cm.GetClientConfigPath()
	templateContent := templates.GetClientTemplate()
	if err := os.WriteFile(configFile, []byte(templateContent), 0600); err != nil {
		return fmt.Errorf("failed to write client config: %w", err)
	}

	fmt.Println("Client configuration reset to defaults.")
	return nil
}

// runConfigClient displays client configuration
func runConfigClient() error {
	cm := pkgConfig.NewConfigManager("zzrouter")

	fmt.Printf("%s Client Configuration\n", ui.GetFolderEmoji())
	fmt.Println("----------------------")
	configFile := cm.GetClientConfigPath()
	fmt.Printf("Config File: %s\n", configFile)

	// Read and display the raw config file content
	content, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read client config file: %w", err)
	}

	fmt.Println("\n--- Configuration Content ---")
	fmt.Println(string(content))
	fmt.Println("-----------------------------")

	return nil
}

// newConfigThemeCmd creates the theme subcommand
func newConfigThemeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "theme [name]",
		Short: "Get or set the UI theme",
		Long: `View or change the UI color theme.

Available themes:
  auto              Auto-detect from terminal (default)
  catppuccin-mocha  Dark theme (Catppuccin Mocha)
  catppuccin-latte  Light theme (Catppuccin Latte)
  tokyo-night       Dark theme (Tokyo Night)

Examples:
  zzrouter config theme                    # Show current theme
  zzrouter config theme catppuccin-latte   # Set light theme
  zzrouter config theme auto               # Auto-detect`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runConfigThemeShow()
			}
			return runConfigThemeSet(args[0])
		},
	}
	return cmd
}

func runConfigThemeShow() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadClientConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	theme := cfg.Preferences.Theme
	if theme == "" {
		theme = "auto"
	}
	fmt.Printf("Current theme: %s\n", theme)
	fmt.Println("\nAvailable themes:")
	for _, t := range []string{"auto", "catppuccin-mocha", "catppuccin-latte", "tokyo-night"} {
		marker := "  "
		if t == theme {
			marker = "* "
		}
		fmt.Printf("  %s%s\n", marker, t)
	}
	return nil
}

func runConfigThemeSet(name string) error {
	// Validate theme name
	valid := map[string]bool{
		"auto": true, "catppuccin-mocha": true, "mocha": true,
		"catppuccin-latte": true, "latte": true,
		"tokyo-night": true, "tokyonight": true,
	}
	if !valid[strings.ToLower(name)] {
		return fmt.Errorf("unknown theme %q: use: auto, catppuccin-mocha, catppuccin-latte, tokyo-night", name)
	}

	cm := pkgConfig.NewConfigManager("zzrouter")
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := store.SetTheme(strings.ToLower(name)); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Theme set to: %s\n", name)
	return nil
}

// runConfigClientEdit opens the client config in an editor
func runConfigClientEdit() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	configFile := cm.GetClientConfigPath()

	fmt.Printf("Editing client configuration...\n")
	fmt.Printf("File: %s\n", configFile)
	fmt.Printf("Editor: %s\n\n", pkgConfig.GetPreferredEditor())

	return pkgConfig.OpenInEditorWithFallback(configFile)
}
