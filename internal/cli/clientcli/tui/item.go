package tui

// Item is the unit a List renders. Views define their own concrete types
// that satisfy Item; the List stays non-generic so rows can be
// heterogeneous within a single view (e.g., group headers + entries).
type Item interface {
	// Title is the primary label shown on the row.
	Title() string

	// Description returns optional secondary text. Empty string = single-line row.
	Description() string

	// FilterValue is what List's filter matches against. Usually Title + Description.
	FilterValue() string
}

// Separator is an Item that cannot be selected. Lists skip it when the
// cursor moves and render it with muted styling.
type Separator interface {
	Item
	Separator() bool
}

// IsSelectable reports whether the item can receive the cursor.
func IsSelectable(it Item) bool {
	s, ok := it.(Separator)
	return !ok || !s.Separator()
}
