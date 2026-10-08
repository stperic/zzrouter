package search

import (
	"strings"

	"charm.land/bubbles/v2/cursor"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewSearchTUIModel creates a new SearchTUIModel with all tag-preset loading logic.
// When embedded is true, ctrl+c and esc-at-root return tuiBackMsg instead of tea.Quit.
func NewSearchTUIModel(styles ui.Styles, query, provider, tagsStr, tagLogic, sortBy string, embedded bool) SearchTUIModel {
	// Load client config to get search tag presets (non-fatal if it fails)
	clientConfig, cfgErr := pkgConfig.LoadClientConfig("zzrouter")
	var tagPresets []TagPreset
	tagDefaultLogic := "AND"
	if cfgErr == nil && clientConfig != nil {
		searchPrefs := clientConfig.Preferences.Search
		if searchPrefs.DefaultLogic != "" {
			tagDefaultLogic = strings.ToUpper(searchPrefs.DefaultLogic)
			if tagDefaultLogic != "AND" && tagDefaultLogic != "OR" {
				tagDefaultLogic = "AND"
			}
		}
		for _, tf := range searchPrefs.ModelTags {
			logic := strings.ToUpper(tf.Logic)
			if logic != "AND" && logic != "OR" {
				logic = tagDefaultLogic
			}
			if len(tf.Tags) == 0 {
				continue
			}
			tagPresets = append(tagPresets, TagPreset{
				Tags:  tf.Tags,
				Logic: logic,
			})
		}
	}

	// Load last used provider from config if not specified
	if provider == "" && cfgErr == nil && clientConfig != nil {
		if saved := clientConfig.Preferences.Search.DefaultRegistry; saved != "" {
			provider = saved
		}
	}

	// Load per-provider sort and tags from saved prefs (if not overridden by CLI)
	if cfgErr == nil && clientConfig != nil && provider != "" {
		if prefs, ok := clientConfig.Preferences.Search.RegistryPrefs[provider]; ok {
			if sortBy == "" && prefs.Sort != "" {
				sortBy = prefs.Sort
			}
			if tagsStr == "" && len(prefs.Tags) > 0 {
				tagsStr = strings.Join(prefs.Tags, ",")
			}
		}
	}

	// Parse tags from CLI parameter or saved prefs
	var initialTags []string
	initialLogic := tagDefaultLogic

	if tagsStr != "" {
		// User provided tags via CLI (or loaded from saved prefs)
		initialTags = strings.Split(tagsStr, ",")
		for i := range initialTags {
			initialTags[i] = strings.TrimSpace(initialTags[i])
		}
		initialLogic = strings.ToUpper(tagLogic)
		if initialLogic != "AND" && initialLogic != "OR" {
			initialLogic = tagDefaultLogic
		}
	} else {
		// No CLI tags
		initialTags = nil
	}

	fc := cursor.New()
	fc.SetChar(" ")

	return SearchTUIModel{
		styles:          styles,
		embedded:        embedded,
		query:           query,
		Provider:        provider,
		sortBy:          sortBy,
		tagFilter:       initialTags,
		tagLogic:        initialLogic,
		tagPresets:      tagPresets,
		tagPresetIndex:  -1,
		tagDefaultLogic: tagDefaultLogic,
		inlineFilter:    query,
		State:           StateLoading,
		listOffset:      0,
		cursor:          0,
		TermWidth:       0,
		TermHeight:      0,
		filterCursor:    fc,
	}
}
