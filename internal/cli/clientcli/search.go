package clientcli

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewSearchCmd creates the search command
func NewSearchCmd() *cobra.Command {
	var (
		provider string
		limit    int
		listMode bool
		tags     string
		tagLogic string
		sortBy   string
	)

	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search for models",
		Long: `Search for models from HuggingFace and Ollama model repositories.

USAGE:
  zzrouter search                            # Browse popular models (interactive)
  zzrouter search <query>                    # Search all repositories (interactive)
  zzrouter search <query> -l                 # List view (table format)
  zzrouter search <query> --provider ollama       # Search specific repository
  zzrouter search <query> --tags gguf        # Filter by tags
  zzrouter search <query> --sort likes       # Sort by likes/downloads/date

INTERACTIVE MODE (Default):
  ↑/↓        Navigate models
  p          Toggle provider (ollama/huggingface)
  m          Edit model filter
  s          Cycle sort options
  t          Cycle tag filters
  Enter      View model details
  q          Quit

TAG FILTERING:
  --tags, -t     Comma-separated list of tags (e.g., gguf,mlx)
  --logic        Tag matching logic: AND or OR (default: AND)
  
  AND logic: Model must have ALL specified tags
  OR logic:  Model must have AT LEAST ONE tag

COMMON TAGS:
  gguf, mlx, safetensors, pytorch, diffusers
  text-generation, image-generation, image-to-image
  en, fr, zh (languages)

SORT OPTIONS (HuggingFace):
  trendingScore - Trending models (default)
  downloads     - Most downloaded
  likes         - Most liked
  lastModified  - Recently updated
  createdAt     - Recently created

EXAMPLES:
  zzrouter search                                    # Browse popular models (interactive)
  zzrouter search llama                              # Search for llama models (interactive)
  zzrouter search --sort likes -l                    # Most liked models (list view)
  zzrouter search "TinyLlama"                        # Interactive search with details
  zzrouter search "TinyLlama" --tags gguf            # Only GGUF models
  zzrouter search "llama" --tags mlx                 # Only MLX models
  zzrouter search --tags text-generation,pytorch --logic OR
  zzrouter search "*chat*" -l --limit 100            # Show top 100 results (list view)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := "" // Default to empty (shows popular models)
			if len(args) > 0 {
				query = args[0]
			}
			if listMode {
				return runSearch(cmd.Context(), query, provider, tags, tagLogic, limit, sortBy)
			}
			return runInteractiveSearch(cmd.Context(), query, provider, tags, tagLogic, sortBy)
		},
	}

	cmd.Flags().StringVar(&provider, "provider", "", "Filter by provider (ollama, huggingface)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of results (list mode only)")
	cmd.Flags().BoolVarP(&listMode, "list", "l", false, "List mode (table format, non-interactive)")
	cmd.Flags().StringVarP(&tags, "tags", "t", "", "Comma-separated list of tags to filter by")
	cmd.Flags().StringVar(&tagLogic, "logic", "AND", "Tag matching logic: AND or OR")
	cmd.Flags().StringVarP(&sortBy, "sort", "s", "trendingScore", "Sort by: trendingScore, downloads, likes, lastModified, createdAt")
	return cmd
}

// runSearch executes the search and displays results in a table
func runSearch(ctx context.Context, query, providerFilter, tagsStr, tagLogic string, limit int, sortBy string) error {
	// Use centralized sign-in logic
	client, err := EnsureConnectedCtx(ctx)
	if err != nil {
		return err
	}

	searchTerm := query
	if searchTerm == "" {
		searchTerm = "popular models"
	}
	fmt.Printf("Searching for '%s'", searchTerm)
	if tagsStr != "" {
		fmt.Printf(" with tags: %s (%s)", tagsStr, tagLogic)
	}
	if sortBy != "trendingScore" {
		fmt.Printf(" (sorted by %s)", sortBy)
	}
	fmt.Println("...")

	// Default provider if not specified
	provider := providerFilter
	if provider == "" {
		provider = "all"
	}

	// Call API endpoint with tags support
	response, err := client.SearchModels(ctx, metadata.SearchQuery{Query: query, Provider: provider, Limit: limit, Sort: sortBy, Order: "desc", Tags: tagsStr, Logic: tagLogic, Full: true, Enrich: true})
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	results := response.Results

	if len(results) == 0 {
		fmt.Println("\nNo results found")
		return nil
	}

	// Get provider from response
	responseProvider := response.Provider

	// Display results
	fmt.Println()
	switch responseProvider {
	case constants.RepoHuggingFace:
		fmt.Printf("%s HuggingFace\n", ui.GetProviderEmoji("huggingface"))
	case constants.RepoOllama:
		fmt.Printf("%s Ollama\n", ui.GetProviderEmoji("ollama"))
	}
	fmt.Println()

	isHuggingFace := responseProvider == constants.RepoHuggingFace || responseProvider == ""
	isOllama := responseProvider == constants.RepoOllama

	// Table header
	if isHuggingFace {
		fmt.Printf("%-50s  %-8s  %-10s  %-8s  %s\n", "MODEL NAME", "TREND", "DOWNLOADS", "LIKES", "UPDATED")
		fmt.Printf("%-50s  %-8s  %-10s  %-8s  %s\n",
			strings.Repeat("─", 50), strings.Repeat("─", 8), strings.Repeat("─", 10), strings.Repeat("─", 8), strings.Repeat("─", 14))
	} else if isOllama {
		fmt.Printf("%-50s  %-10s  %s\n", "MODEL NAME", "DOWNLOADS", "UPDATED")
		fmt.Printf("%-50s  %-10s  %s\n",
			strings.Repeat("─", 50), strings.Repeat("─", 10), strings.Repeat("─", 14))
	} else {
		fmt.Printf("%-50s  %s\n", "MODEL NAME", "DOWNLOADS")
		fmt.Printf("%-50s  %s\n", strings.Repeat("─", 50), strings.Repeat("─", 10))
	}

	// Display results
	for _, entry := range results {
		result := entry.Discovery
		modelID := result.DisplayName
		if len(modelID) > 50 {
			modelID = modelID[:47] + "..."
		}
		downloads, likes, trendingScore := result.Downloads, result.Likes, result.TrendingScore
		modifiedAt := result.ModifiedAt

		updatedDisplay := shared.EmptyValue
		if modifiedAt != "" {
			updatedDisplay = search.FormatRelativeTimeFromISO(modifiedAt)
		}

		if isHuggingFace {
			trendDisplay := shared.EmptyValue
			if trendingScore > 0 {
				trendDisplay = fmt.Sprintf("%.0f", trendingScore)
			}
			fmt.Printf("%-50s  %-8s  %-10s  %-8s  %s\n",
				modelID, trendDisplay, shared.FormatSearchDownloads(downloads), shared.FormatSearchDownloads(likes), updatedDisplay)
		} else if isOllama {
			fmt.Printf("%-50s  %-10s  %s\n",
				modelID, shared.FormatSearchDownloads(downloads), updatedDisplay)
		} else {
			fmt.Printf("%-50s  %s\n",
				modelID, shared.FormatSearchDownloads(downloads))
		}
	}

	fmt.Printf("\nShowing %d result(s)\n", len(results))

	return nil
}

// runInteractiveSearch starts the interactive search TUI
func runInteractiveSearch(ctx context.Context, query, provider, tagsStr, tagLogic, sortBy string) error {
	client, err := EnsureConnectedCtx(ctx)
	if err != nil {
		return err
	}

	theme := shared.ResolveUITheme()
	styles := ui.NewStyles(theme)

	m := search.NewSearchTUIModel(client, styles, query, provider, tagsStr, tagLogic, sortBy, false)

	p := tea.NewProgram(m, tea.WithContext(ctx))
	defer m.Deactivate()

	defer func() {
		fmt.Print("\033[?25h")
		fmt.Print("\033[0m")
		fmt.Print("\033[?1049l")
		fmt.Print("\r\n")
		_ = os.Stdout.Sync()
	}()

	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}
