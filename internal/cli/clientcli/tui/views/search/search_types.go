package search

import (
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// UI State
type viewState int

const (
	StateLoading viewState = iota
	StateList
	stateVariantSelect
	StateDetail
	stateProviderSelect
	statePulling
	stateFilterEdit
	stateNodeSelect
	statePickerOverlay
	stateDirectPull
	stateTagAdd
)

// searchProviderConfig defines display and sort options for a search provider.
type searchProviderConfig struct {
	Icon         string
	DisplayName  string
	SortOptions  []string        // ordered cycle; first entry is the default
	ClientSorts  map[string]bool // sorts handled client-side (no re-fetch)
	ClientFilter bool            // true = name filtering uses cached results (no re-fetch)
	HasTags      bool            // true = supports tag-based filtering
	IsCloud      bool            // true = cloud API provider (no local download needed)
	Configured   bool            // true = provider exists in provider config
	Enabled      bool            // true = provider is configured AND enabled for inference
}

// DefaultSearchProviders is empty — all provider configs are server-driven
// via parseSearchProviders. No hardcoded providers in the client.
var DefaultSearchProviders = map[string]searchProviderConfig{}

// defaultProviderKeys is empty — populated from server response.
var defaultProviderKeys []string

// getProviderConfig returns the config for the given provider key.
// It checks the model's server-driven configs first, then the defaults.
// Empty string defaults to the first available provider.
func (m SearchTUIModel) getProviderConfig(provider string) searchProviderConfig {
	if provider == "" {
		if len(m.providerKeys) > 0 {
			provider = m.providerKeys[0]
		} else {
			provider = constants.RepoHuggingFace
		}
	}
	var cfg searchProviderConfig
	var found bool
	if m.providerConfigs != nil {
		cfg, found = m.providerConfigs[provider]
	}
	if !found {
		cfg, found = DefaultSearchProviders[provider]
	}
	if !found {
		cfg = searchProviderConfig{DisplayName: provider, SortOptions: []string{"newest", "name"}, ClientSorts: map[string]bool{"name": true, "newest": true}}
	}
	// Always resolve icon from the single source of truth
	cfg.Icon = ui.GetProviderEmoji(provider)
	return cfg
}

// isCloudVariantSelected returns true if the currently selected variant is a cloud tag.
// Used to route cloud variants through the same registration path as cloud providers.
func (m SearchTUIModel) isCloudVariantSelected() bool {
	if m.selectedVariant == nil {
		return false
	}
	name := strings.ToLower(m.selectedVariant.Filename)
	return name == "cloud" || strings.HasSuffix(name, "-cloud")
}

// parseSearchProviders converts the server's apps response into the client
// types used by the TUI. Returns an ordered list of provider keys and a
// config map.
func parseSearchProviders(data []map[string]any) ([]string, map[string]searchProviderConfig) {
	keys := make([]string, 0, len(data))
	configs := make(map[string]searchProviderConfig, len(data))

	for _, entry := range data {
		name, _ := entry["name"].(string)
		if name == "" {
			continue
		}

		cfg := searchProviderConfig{}

		if v, ok := entry["display_name"].(string); ok && v != "" {
			cfg.DisplayName = v
		} else {
			cfg.DisplayName = name
		}

		if arr, ok := entry["sort_options"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					cfg.SortOptions = append(cfg.SortOptions, s)
				}
			}
		}
		if len(cfg.SortOptions) == 0 {
			cfg.SortOptions = []string{"newest", "name"}
		}

		cfg.ClientSorts = make(map[string]bool)
		if arr, ok := entry["client_sorts"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					cfg.ClientSorts[s] = true
				}
			}
		}

		if v, ok := entry["client_filter"].(bool); ok {
			cfg.ClientFilter = v
		}

		if v, ok := entry["has_tags"].(bool); ok {
			cfg.HasTags = v
		}

		if v, ok := entry["is_cloud"].(bool); ok {
			cfg.IsCloud = v
		}

		if v, ok := entry["configured"].(bool); ok {
			cfg.Configured = v
		}

		if v, ok := entry["enabled"].(bool); ok {
			cfg.Enabled = v
		}

		keys = append(keys, name)
		configs[name] = cfg
	}

	return keys, configs
}

// defaultSort returns the first sort option for this provider.
func (cfg searchProviderConfig) defaultSort() string {
	if len(cfg.SortOptions) > 0 {
		return cfg.SortOptions[0]
	}
	return "name"
}

// KeyValue is an ordered metadata pair for display in the detail view.
type KeyValue struct {
	Key   string // Display-ready label (e.g., "Max Output", "Mode")
	Value string // Pre-formatted for display
}

// searchResult represents a unified search result
type searchResult struct {
	// Typed fields for list view columns + sorting
	Provider      string
	Name          string // Display name (clean, for UI)
	RawID         string // Raw provider ID (for API operations like deploy, card)
	Description   string
	Downloads     int
	Likes         int      // Number of likes (HuggingFace)
	CreatedAt     string   // Creation date
	ModifiedAt    string   // Last modified date (Ollama)
	Tags          []string // Model tags (gguf, mlx, text-generation, etc.)
	SizeBytes     int64    // Size in bytes
	SizeDisplay   string   // Formatted size (e.g., "2.5 GB")
	URL           string
	VariantCount  int     // Number of variants/quantizations available
	ContextWindow string  // Context window size (e.g., "8K", "128K")
	ContextLength int     // Raw context length in tokens (for sorting)
	TrendingScore float64 // HuggingFace trending score
	PricePrompt   float64 // Price per token (prompt/input) — cloud providers
	PriceComplete float64 // Price per token (completion/output) — cloud providers
	PriceDisplay  string  // Formatted price (e.g., "$0.50/M")
	HasCloud      bool    // True if model has cloud-served variants (Ollama)

	// Flexible metadata for detail view — built lazily from rawData
	Metadata []KeyValue
	rawData  map[string]any // Raw server response, used to build Metadata on demand
}

// APIName returns the model identifier for API operations (deploy, card, chat).
// Uses RawID (set from server's api_id) when available, falls back to Name.
func (r *searchResult) APIName() string {
	if r.RawID != "" {
		return r.RawID
	}
	return r.Name
}

// TagPreset represents a reusable tag filter preset loaded from client config
type TagPreset struct {
	Tags  []string // Tags to apply (e.g., ["gguf"], ["mlx"], etc.)
	Logic string   // AND or OR
}

// ModelVariant represents a quantization variant
type ModelVariant struct {
	Filename    string
	Size        int64
	SizeDisplay string
	QuantMethod string
	Description string
}

// SearchTUIModel is the Bubble Tea model for interactive search
type SearchTUIModel struct {
	// Theme styles (immutable after init)
	styles ui.Styles

	// Embedded mode: when true, ctrl+c and esc-at-root return tui.TuiBackMsg instead of tea.Quit
	embedded bool

	query     string
	Provider  string
	sortBy    string
	tagFilter []string // Active tags (from CLI or current preset)
	tagLogic  string   // AND or OR for active tags

	// Tag presets loaded from client config (preferences.search.model_tags)
	tagPresets       []TagPreset
	tagPresetIndex   int            // -1 = no preset (custom or no filter), 0..N-1 = preset index
	tagDefaultLogic  string         // Default logic when no preset is active
	tagAddInput      string         // Text input for adding a new tag preset
	allResults       []searchResult // Full unfiltered results from last fetch
	results          []searchResult // Filtered/sorted view of allResults
	cursor           int
	listOffset       int
	State            viewState
	prevState        viewState // tracks where we came from (for Esc navigation)
	err              error
	selectedItem     *searchResult
	PullStatus       string
	detailScroll     int
	variants         []ModelVariant
	variantCursor    int
	variantOffset    int
	variantSort      string // "recommended" (default), "name", "size"
	selectedVariant  *ModelVariant
	providerOptions  []shared.DownloadOption
	providerCursor   int
	selectedProvider string

	// Filter edit state
	filterInput   string
	inlineFilter  string // Always-visible inline filter at bottom of list
	broadSearch   bool   // true when ~ prefix used (search name + description + tags)
	debounceSeqNo int    // Incremented on each keystroke, debounce only fires if current

	// Node selection state
	availableNodes []hostWithProviders
	hostCursor     int
	selectedNodes  map[string]bool // multi-select: toggled nodes for deploy

	// Animation
	shimmerFrame int

	// Terminal dimensions
	TermWidth  int
	TermHeight int

	// Node count for title bar (updated by parent TUI)
	NodeCount int

	// Blinking cursor for the filter input
	filterCursor cursor.Model

	// Cached glamour-rendered description (computed once when details load)
	renderedDesc string

	// Direct deploy overlay (Ctrl+D)
	directPullInput   string
	directPullError   string // Validation error shown inside the overlay
	directPullLoading bool   // True while validating model name

	// Modal error popup (ui.Popup). When non-nil, overlays the active view
	// and swallows keystrokes until dismissed.
	errorPopup *ui.Popup

	// Server-driven provider metadata (populated by providerConfigsLoadedMsg)
	providerKeys    []string                        // ordered list of provider keys for cycling
	providerConfigs map[string]searchProviderConfig // provider key → config

	// Picker overlay state
	pickerTitle  string   // Title of the popup (e.g., "Sort by", "Provider", "Tags")
	pickerItems  []string // Display labels
	pickerKeys   []string // Internal keys/values (same length as items)
	pickerCursor int      // Current selection
	pickerAction string   // What the picker is for: "sort", "provider", "tags"
}

// debounceSearchMsg is sent after the debounce delay to trigger a new search
type debounceSearchMsg struct {
	query string // The query at the time the debounce was scheduled
}

// Messages
type searchCompleteMsg struct {
	results []searchResult
	err     error
}

type deployCompleteMsg struct {
	success bool
	err     error
}

type modelDetailsMsg struct {
	description string
	metadata    []KeyValue
	err         error
}

type variantsLoadedMsg struct {
	variants []ModelVariant
	err      error
}

type providerLoadedMsg struct {
	options []shared.DownloadOption
	err     error
}

type hostWithProviders struct {
	name            string
	providers       []string // List of provider types available on this host
	diskAvailableGB float64
	diskTotalGB     float64
	ramAvailableGB  float64
	ramTotalGB      float64
	vramAvailableGB float64
	vramTotalGB     float64
	gpuCount        int
	role            string // coordinator/worker
}

type hostsLoadedMsg struct {
	hosts []hostWithProviders
	err   error
}

// providerConfigsLoadedMsg carries provider metadata fetched from the server.
type providerConfigsLoadedMsg struct {
	keys    []string
	configs map[string]searchProviderConfig
}

// cloudModelAddedMsg signals that a cloud model registration completed.
type cloudModelAddedMsg struct {
	modelID string
	err     error
}

// deployValidateMsg carries the result of a pre-deploy model validation.
type deployValidateMsg struct {
	modelName string
	provider  string
	err       error
}

// listPageSize returns how many data rows fit in the search list view.
func (m SearchTUIModel) listPageSize() int {
	footer := m.renderFooter("")
	vis := shared.VisibleRows(m.TermHeight, footer)
	if vis > 50 {
		return 50
	}
	return vis
}

// variantPageSize returns how many variant rows fit in the variant select view.
func (m SearchTUIModel) variantPageSize() int {
	footer := shared.RenderViewFooterWithStatus(m.styles, m.TermWidth, " ", " ") + "\n"
	return shared.VisibleRows(m.TermHeight, footer)
}

// Init initializes the model
func (m SearchTUIModel) Init() tea.Cmd {
	return tea.Batch(m.fetchResults(), m.filterCursor.Focus(), m.fetchProviderConfigs())
}

// quitOrBack returns a tea.Cmd that either quits (standalone) or goes back to the
// parent menu (embedded mode).
func (m SearchTUIModel) quitOrBack() tea.Cmd {
	if m.embedded {
		return func() tea.Msg { return tui.TuiBackMsg{} }
	}
	return tea.Quit
}
