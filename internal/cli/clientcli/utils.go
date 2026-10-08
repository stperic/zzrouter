package clientcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/pkg/apipath"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	pkgUtils "github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// isStdinTTY reports whether stdin is attached to a terminal.
// Used to gate interactive confirmation prompts — scripted / CI invocations
// pipe stdin and should skip the prompt rather than block on fmt.Scanln.
func isStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmYN prompts the user with a Y/N question and returns true iff
// the user typed "y" or "yes" (case-insensitive). When stdin is not a
// TTY (piped, CI), deny without blocking on fmt.Scanln.
func confirmYN(prompt string) bool {
	if !isStdinTTY() {
		return false
	}
	fmt.Print(prompt)
	var response string
	_, _ = fmt.Scanln(&response)
	r := strings.ToLower(strings.TrimSpace(response))
	return r == "y" || r == "yes"
}

// readLine reads a single line from stdin for interactive prompts
// (API key paste, numeric selection). Returns "" on non-TTY or empty
// input; empty-means-abort at call sites.
func readLine(prompt string) string {
	if !isStdinTTY() {
		return ""
	}
	fmt.Print(prompt)
	var response string
	_, _ = fmt.Scanln(&response)
	return strings.TrimSpace(response)
}

// OutputFormat represents the output format type
type OutputFormat string

const (
	OutputTable OutputFormat = "table"
	OutputJSON  OutputFormat = "json"
	OutputYAML  OutputFormat = "yaml"
)

// GetOutputFormat returns the output format from the command's persistent flags
func GetOutputFormat(cmd *cobra.Command) OutputFormat {
	if cmd == nil {
		return OutputTable
	}

	// Check the global --output flag
	output, _ := cmd.Root().PersistentFlags().GetString("output")
	switch strings.ToLower(output) {
	case "json":
		return OutputJSON
	case "yaml":
		return OutputYAML
	case "table", "":
		return OutputTable
	default:
		return OutputTable
	}
}

// OutputData outputs data in the specified format
// data should be a struct or slice that can be marshaled to JSON/YAML
func OutputData(cmd *cobra.Command, data any, tableRenderer func()) error {
	format := GetOutputFormat(cmd)

	switch format {
	case OutputJSON:
		jsonBytes, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to format JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil

	case OutputYAML:
		yamlBytes, err := yamlLikeJSON(data)
		if err != nil {
			return fmt.Errorf("failed to format YAML: %w", err)
		}
		fmt.Print(string(yamlBytes))
		return nil

	default: // OutputTable
		tableRenderer()
		return nil
	}
}

// yamlLikeJSON renders data as YAML with the shape its JSON has. The
// client's types are tagged for JSON only: marshalled directly, an
// embedded struct would nest under its type name and omitempty fields
// would print empty, so -o yaml and -o json would describe different
// documents. JSON is YAML, so its decoded node tree re-encodes in block
// style with JSON's keys in JSON's order.
func yamlLikeJSON(data any) ([]byte, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(jsonBytes, &node); err != nil {
		return nil, err
	}
	blockStyle(&node)
	return yaml.Marshal(&node)
}

// blockStyle clears the flow and quoting styles JSON input leaves on
// every node; the encoder quotes again only where a plain scalar would
// read as another type.
func blockStyle(n *yaml.Node) {
	n.Style &^= yaml.FlowStyle | yaml.DoubleQuotedStyle
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// ParsePositionalModelArg handles positional model arguments consistently
func ParsePositionalModelArg(args []string, model string) (string, error) {
	if len(args) == 1 {
		if model != "" {
			return "", fmt.Errorf("cannot specify both positional model name and --model flag")
		}
		return args[0], nil
	}
	return model, nil
}

// AddCommonFilterFlags adds the standard node/provider flags to a command (DRY approach)
func AddCommonFilterFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("provider", "a", "", "Provider: vllm, ollama, llamacpp, mlx (defaults to * for all providers)")
	cmd.Flags().StringP("node", "n", "", "Node name (defaults to * for all nodes)")
}

// FilterInstancesBySpec filters instances by model name OR instance ID (DRY utility)
// Supports:
// - Instance ID (exact or prefix match): "06c1097efce0" or "06c109"
// - Model name (exact match): "qwen3-coder:30b"
// - Model name (substring match): "qwen"
// - Model name (wildcard pattern): "Qwen*"
func FilterInstancesBySpec(instances []pkgClient.Instance, spec string) []pkgClient.Instance {
	if spec == "" {
		return instances
	}

	var filtered []pkgClient.Instance
	specLower := strings.ToLower(spec)
	isWildcard := strings.HasSuffix(spec, "*")
	pattern := strings.TrimSuffix(specLower, "*")

	for _, inst := range instances {
		// Try to match by instance ID first (exact or prefix)
		if strings.HasPrefix(strings.ToLower(inst.ID), pattern) {
			filtered = append(filtered, inst)
			continue
		}

		// Try to match by model name
		modelLower := strings.ToLower(inst.Model)
		if isWildcard {
			// Wildcard pattern (e.g., "Qwen*")
			if strings.HasPrefix(modelLower, pattern) {
				filtered = append(filtered, inst)
			}
		} else {
			// Exact or substring match
			if modelLower == specLower || strings.Contains(modelLower, specLower) {
				filtered = append(filtered, inst)
			}
		}
	}

	return filtered
}

// healthCheckCache caches successful health checks to avoid redundant network calls
var healthCheckCache = struct {
	sync.RWMutex
	lastCheck map[string]time.Time // host address -> last successful check time
	ttl       time.Duration
}{
	lastCheck: make(map[string]time.Time),
	ttl:       constants.CacheRefreshInterval, // Cache health checks for 30 seconds
}

// EnsureConnected ensures the user is signed in to a host. Background
// context — for cancellable callers, prefer EnsureConnectedCtx.
//
// This is the centralized sign-in logic for all CLI commands. Returns
// a client if signed in successfully, or an error with sign-in
// guidance. Health checks are cached for 30 seconds to avoid redundant
// network calls.
func EnsureConnected() (*pkgClient.Client, error) {
	return EnsureConnectedCtx(context.Background())
}

// EnsureConnectedCtx is the cancellable form of EnsureConnected. The
// supplied ctx is threaded through the health-check HTTP request so a
// hung node doesn't pin the CLI command past its operator-supplied
// deadline.
func EnsureConnectedCtx(ctx context.Context) (*pkgClient.Client, error) {
	cm := pkgConfig.NewConfigManager("zzrouter")
	clientConfig, err := cm.LoadClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load client configuration: %w", err)
	}

	// Check if node is configured
	if clientConfig.Node.Address == "" {
		return nil, fmt.Errorf("not connected to a zzRouter node\n\n  Connect to a node first:\n    zzrouter connect <host:port>\n    zzrouter connect              (auto-discover on local network)")
	}

	// Get host config
	host, err := clientConfig.GetNodeConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid connection config: %w\n\n  Reconnect with: zzrouter connect <host:port>", err)
	}

	// Check health check cache
	healthCheckCache.RLock()
	lastCheck, exists := healthCheckCache.lastCheck[host.Address]
	healthCheckCache.RUnlock()

	if exists && time.Since(lastCheck) < healthCheckCache.ttl {
		return pkgClient.NewClient(*host), nil
	}

	// Perform health check
	healthURL := host.Address + apipath.Health
	httpClient := &http.Client{Timeout: constants.HTTPShortTimeout}

	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create health request: %w", err)
	}

	if host.APIKey != "" {
		req.Header.Set("X-API-Key", host.APIKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach zzRouter node at %s\n\n  Make sure zzrouter-node is running, or reconnect:\n    zzrouter connect <host:port>", host.Name)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("authentication failed (invalid API key)\n\n  Reconnect with a valid key:\n    zzrouter connect %s", host.Address)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zzRouter node at %s returned status %d\n\n  Check if zzrouter-node is healthy, or reconnect:\n    zzrouter connect <host:port>", host.Name, resp.StatusCode)
	}

	// Health check successful - update cache
	healthCheckCache.Lock()
	healthCheckCache.lastCheck[host.Address] = pkgUtils.Now()
	healthCheckCache.Unlock()

	return pkgClient.NewClient(*host), nil
}

// ParseModelQueryParams parses and normalizes model query parameters from flags and args
func ParseModelQueryParams(cmd *cobra.Command, args []string) (*shared.ModelQueryParams, error) {
	params := &shared.ModelQueryParams{}

	params.Node, _ = cmd.Flags().GetString("node")
	params.Registry, _ = cmd.Flags().GetString("registry")
	params.Provider, _ = cmd.Flags().GetString("provider")
	params.Model, _ = cmd.Flags().GetString("model")
	params.SortBy, _ = cmd.Flags().GetString("sort")

	if len(args) > 0 && params.Model == "" {
		params.Model = args[0]
	}

	return params, nil
}

// ConvertModelsForRunCommand converts ModelMetadata to ModelInfo for start command (DRY utility)
func ConvertModelsForRunCommand(models []pkgClient.ModelMetadata) []ModelInfo {
	var result []ModelInfo
	for _, m := range models {
		// Use AssignedApp (what will run it) not SourceRepo (where it came from)
		providerToUse := m.AssignedApp
		if providerToUse == "" {
			// Only fallback to SourceRepo for Ollama models (they manage themselves)
			// For HuggingFace and others, leave empty (user will select manually)
			if m.SourceRepo == constants.RepoOllama {
				providerToUse = constants.AppOllama
			}
			// Otherwise leave empty - will show as "-" in display
		}

		// Use GetModifiedTime() to handle both modified_at and modified fields
		modTime := m.GetModifiedTime()
		modifiedStr := ""
		if !modTime.IsZero() {
			modifiedStr = modTime.Format("2006-01-02")
		}

		result = append(result, ModelInfo{
			Node:     m.Node,
			Provider: providerToUse, // Provider that will run it (mlx, test, etc.) or empty
			Name:     m.Name,
			FullID:   m.Name, // Store original name for loading
			Size:     shared.FormatSize(m.Size),
			Modified: modifiedStr,
		})
	}
	return result
}

// ConvertModelsForRmCommand converts ModelMetadata to ModelInfo for rm command (DRY utility)
func ConvertModelsForRmCommand(models []pkgClient.ModelMetadata) []ModelInfo {
	var result []ModelInfo
	for _, m := range models {
		// Determine source repo - preserve original, infer from format if empty
		sourceRepo := m.SourceRepo
		if sourceRepo == "" {
			// Infer from format - GGUF files are typically from HuggingFace
			format := m.GetFormat()
			if format == "gguf" {
				sourceRepo = constants.RepoHuggingFace
			} else {
				// For other formats, leave empty - server will handle it
				sourceRepo = ""
			}
		}

		// Use GetModifiedTime() to handle both modified_at and modified fields
		modTime := m.GetModifiedTime()
		modifiedStr := ""
		if !modTime.IsZero() {
			modifiedStr = modTime.Format("2006-01-02")
		}

		// Don't add emoji here - it will be added by the display function
		result = append(result, ModelInfo{
			Node:     m.Node,
			Provider: sourceRepo,
			Name:     m.Name,
			FullID:   m.Name, // Store original name
			Size:     shared.FormatSize(m.Size),
			Modified: modifiedStr,
		})
	}
	return result
}

// TableRow represents a row in the unified table display (DRY utility)
type TableRow struct {
	ID       int    // Auto-generated ID for interactive selection (optional)
	Node     string // Node name
	Provider string // Provider type (for emoji)
	Model    string
	Format   string // Model format (mlx, gguf, safetensors, etc.)
	Size     string
	Modified string
}

// displayUnifiedTableFromRows displays a unified table from pre-built rows (DRY utility)
func displayUnifiedTableFromRows(rows []TableRow) {
	if len(rows) == 0 {
		return
	}

	// Add spacing before table
	fmt.Println()

	// Calculate column widths
	maxNodeWidth := len("NODE")
	maxModelWidth := len("MODEL")
	maxFormatWidth := len("FORMAT")
	maxSizeWidth := len("SIZE")
	maxModifiedWidth := len("MODIFIED")

	for _, row := range rows {
		// Calculate max widths using visual width (handles emojis properly)
		if nodeWidth := runewidth.StringWidth(row.Node); nodeWidth > maxNodeWidth {
			maxNodeWidth = nodeWidth
		}
		if modelWidth := runewidth.StringWidth(row.Model); modelWidth > maxModelWidth {
			maxModelWidth = modelWidth
		}
		if formatWidth := runewidth.StringWidth(row.Format); formatWidth > maxFormatWidth {
			maxFormatWidth = formatWidth
		}
		if sizeWidth := runewidth.StringWidth(row.Size); sizeWidth > maxSizeWidth {
			maxSizeWidth = sizeWidth
		}
		if modifiedWidth := runewidth.StringWidth(row.Modified); modifiedWidth > maxModifiedWidth {
			maxModifiedWidth = modifiedWidth
		}
	}

	// Print header
	fmt.Printf("%-*s  %-*s  %-*s  %-*s  %-*s\n",
		maxNodeWidth, "NODE",
		maxModelWidth, "MODEL",
		maxFormatWidth, "FORMAT",
		maxSizeWidth, "SIZE",
		maxModifiedWidth, "MODIFIED")

	// Print separator line
	fmt.Printf("%s  %s  %s  %s  %s\n",
		strings.Repeat("-", maxNodeWidth),
		strings.Repeat("-", maxModelWidth),
		strings.Repeat("-", maxFormatWidth),
		strings.Repeat("-", maxSizeWidth),
		strings.Repeat("-", maxModifiedWidth))

	// Print data rows
	for _, row := range rows {
		// Calculate padding for each column to handle visual width properly
		nodePadding := maxNodeWidth - runewidth.StringWidth(row.Node)
		modelPadding := maxModelWidth - runewidth.StringWidth(row.Model)
		formatPadding := maxFormatWidth - runewidth.StringWidth(row.Format)
		sizePadding := maxSizeWidth - runewidth.StringWidth(row.Size)

		fmt.Printf("%s%s  %s%s  %s%s  %s%s  %s\n",
			row.Node, strings.Repeat(" ", nodePadding),
			row.Model, strings.Repeat(" ", modelPadding),
			row.Format, strings.Repeat(" ", formatPadding),
			row.Size, strings.Repeat(" ", sizePadding),
			row.Modified)
	}
}

// ModelInfo represents extracted model information for display and operations (DRY utility)
type ModelInfo struct {
	Node     string
	Provider string
	Name     string // Display name (shown in table)
	FullID   string // Full identifier for API operations (org/model/filename for GGUF, org/model for HF)
	Size     string
	Modified string
}

// ConvertModelInfoToTableRows converts model info to table rows for display (DRY utility)
func ConvertModelInfoToTableRows(models []ModelInfo) []TableRow {
	rows := make([]TableRow, len(models))
	for i, model := range models {
		formattedModelName := pkgUtils.FormatModelName(model.Name)
		rows[i] = TableRow{
			Node:     model.Node,
			Provider: model.Provider,
			Model:    shared.FormatModelWithRegistry(model.Provider, formattedModelName),
			Size:     model.Size,
			Modified: model.Modified,
		}
	}
	return rows
}

// formatUntilFromKeepAlive calculates "until" time from KeepAlive and activity time
// KeepAlive is a duration string (e.g., "5m", "1h")
// lastActivity is the last activity timestamp, startedAt is fallback if no activity yet
func formatUntilFromKeepAlive(keepAlive, lastActivity, startedAt string) string {
	// If keep_alive is missing, return dash
	if keepAlive == "" {
		return "-"
	}

	// Use lastActivity if available, otherwise fall back to startedAt
	activityTime := lastActivity
	if activityTime == "" {
		activityTime = startedAt
	}
	if activityTime == "" {
		return "-"
	}

	// Parse keep-alive duration
	duration, err := time.ParseDuration(keepAlive)
	if err != nil {
		return "-"
	}

	// Negative duration or very large duration (>100 years) means "keep forever"
	// Ollama uses year 2318 (~293 years) to indicate indefinite keep-alive
	if duration < 0 || duration > 100*365*24*time.Hour {
		return "Forever"
	}

	// Parse activity time (RFC3339 format)
	parsedActivityTime, err := time.Parse(time.RFC3339, activityTime)
	if err != nil {
		return "-"
	}

	// Calculate expiration time
	expiresAt := parsedActivityTime.Add(duration)

	// Calculate time until expiration
	now := pkgUtils.Now()
	timeUntil := expiresAt.Sub(now)

	// If expired or very close to expiring (< 1 second), show dash
	// This handles cases where the instance is still running but timestamp is stale
	if timeUntil <= time.Second {
		return "-"
	}

	// Format as relative time (reuse existing helper)
	return formatDurationUntil(timeUntil)
}

// formatDurationUntil formats a duration as "X minutes/hours/days from now"
func formatDurationUntil(duration time.Duration) string {
	if duration < time.Minute {
		return "less than a minute"
	}

	if duration < time.Hour {
		// Round up to nearest minute
		minutes := int((duration + time.Minute - 1) / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}

	if duration < 24*time.Hour {
		// Round up to nearest hour
		hours := int((duration + time.Hour - 1) / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}

	// Round up to nearest day
	days := int((duration + 24*time.Hour - 1) / (24 * time.Hour))
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// withClient connects before running a subcommand. The run functions it
// wraps take the narrow interface they use, so tests hand them a fake.
func withClient(run func(*cobra.Command, *pkgClient.Client, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		client, err := EnsureConnected()
		if err != nil {
			return err
		}
		return run(cmd, client, args)
	}
}
