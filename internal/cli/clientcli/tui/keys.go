package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
)

// MenuKeyMap defines key bindings for the main menu.
type MenuKeyMap struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Quit  key.Binding
}

var MenuKeys = MenuKeyMap{
	Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "select")),
	Quit:  key.NewBinding(key.WithKeys("q", "Q", "ctrl+c"), key.WithHelp("Q", "quit")),
}

func (k MenuKeyMap) HintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "select"),
		Hint(k.Quit, "quit"),
	)
}

// Standard hint order: navigate > page > enter > [actions] > back

// ListKeyMap is the shared navigation keymap every list/detail view reuses
// for cursor motion and exit. Domain actions (Enter/Refresh/per-view
// operations) live in the view's own keymap — don't add them here.
type ListKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Back     key.Binding
	Quit     key.Binding
}

var ListKeys = ListKeyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

// LogsKeyMap carries logs-specific domain actions. Shared nav bindings
// (Up/Down/PgUp/PgDn/Home/End/Back/Quit) come from ListKeys.
type LogsKeyMap struct {
	Enter   key.Binding
	Follow  key.Binding
	Refresh key.Binding
	Payload key.Binding
	Open    key.Binding
	Copy    key.Binding
	Save    key.Binding
}

var LogsKeys = LogsKeyMap{
	Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	Follow:  key.NewBinding(key.WithKeys("f", "F"), key.WithHelp("F", "follow")),
	Refresh: key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Payload: key.NewBinding(key.WithKeys("p", "P"), key.WithHelp("P", "payload")),
	Open:    key.NewBinding(key.WithKeys("o", "O", "enter"), key.WithHelp("O/Enter", "open")),
	Copy:    key.NewBinding(key.WithKeys("c", "C"), key.WithHelp("C", "copy")),
	Save:    key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "save")),
}

// ProvidersKeyMap defines key bindings for the apps view (list + detail).
type ProvidersKeyMap struct {
	Up        key.Binding
	Down      key.Binding
	PageUp    key.Binding
	PageDown  key.Binding
	Home      key.Binding
	End       key.Binding
	Enter     key.Binding
	Edit      key.Binding
	Install   key.Binding
	Uninstall key.Binding
	Upgrade   key.Binding
	Logs      key.Binding
	Refresh   key.Binding
	Back      key.Binding
	Quit      key.Binding
}

var ProvidersKeys = ProvidersKeyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:    key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:      key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:       key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	Edit:      key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("E", "parameters")),
	Install:   key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("A", "install")),
	Uninstall: key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "uninstall")),
	Upgrade:   key.NewBinding(key.WithKeys("u", "U"), key.WithHelp("U", "upgrade")),
	Logs:      key.NewBinding(key.WithKeys("l", "L"), key.WithHelp("L", "logs")),
	Refresh:   key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:      key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func (k ProvidersKeyMap) ListHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "details"),
		Hint(k.Install, "install"),
		Hint(k.Uninstall, "uninstall"),
		Hint(k.Upgrade, "upgrade"),
		Hint(k.Logs, "logs"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "back"),
	)
}

func (k ProvidersKeyMap) DetailHintsString() string {
	return JoinHints(
		Hint(k.Up, "scroll"),
		Hint(k.Edit, "parameters"),
		Hint(k.Back, "back"),
	)
}

// ModelsKeyMap defines key bindings for the models list view.
type ModelsKeyMap struct {
	Up        key.Binding
	Down      key.Binding
	PageUp    key.Binding
	PageDown  key.Binding
	Home      key.Binding
	End       key.Binding
	Enter     key.Binding
	Edit      key.Binding
	Chat      key.Binding
	Test      key.Binding
	StartStop key.Binding
	Delete    key.Binding
	Refresh   key.Binding
	Back      key.Binding
	Quit      key.Binding
	Sort      key.Binding
	// Route-specific
	New       key.Binding
	Space     key.Binding
	Right     key.Binding
	Left      key.Binding
	AddDeploy key.Binding
}

var ModelsKeys = ModelsKeyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:    key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:      key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:       key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	Edit:      key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("E", "parameters")),
	Chat:      key.NewBinding(key.WithKeys("c", "C"), key.WithHelp("C", "chat")),
	Test:      key.NewBinding(key.WithKeys("t", "T"), key.WithHelp("T", "test")),
	StartStop: key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "start/stop")),
	Delete:    key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "delete")),
	Refresh:   key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Sort:      key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("^S", "sort")),
	Back:      key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	New:       key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("N", "new route")),
	Space:     key.NewBinding(key.WithKeys("space"), key.WithHelp("Space", "expand")),
	Right:     key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→", "expand")),
	Left:      key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←", "collapse")),
	AddDeploy: key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("A", "add deploy")),
}

// listHintsForRow returns context-sensitive hints based on the selected row type.
// Cloud models: no start/stop, show expand (for routes)
// Local models: show start/stop, no expand
// Routes: show expand only

func (k ModelsKeyMap) RouteDetailHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Edit, "edit route"),
		Hint(k.AddDeploy, "add deploy"),
		Hint(k.Delete, "remove deploy"),
		Hint(k.Back, "back"),
	)
}

// RunlogsKeyMap defines key bindings for the run logs view (picker + viewer).
type RunlogsKeyMap struct {
	Up        key.Binding
	Down      key.Binding
	PageUp    key.Binding
	PageDown  key.Binding
	Home      key.Binding
	End       key.Binding
	Enter     key.Binding
	Top       key.Binding // g
	Bottom    key.Binding // G
	Follow    key.Binding // f/F
	Refresh   key.Binding // r/R
	Search    key.Binding // /
	NextMatch key.Binding // n
	PrevMatch key.Binding // N
	Back      key.Binding
	Quit      key.Binding
}

var RunlogsKeys = RunlogsKeyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "scroll")),
	Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll")),
	PageUp:    key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page")),
	Home:      key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:       key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "open")),
	Top:       key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
	Bottom:    key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom")),
	Follow:    key.NewBinding(key.WithKeys("f", "F"), key.WithHelp("F", "follow")),
	Refresh:   key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Search:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	NextMatch: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next match")),
	PrevMatch: key.NewBinding(key.WithKeys("N"), key.WithHelp("N", "prev match")),
	Back:      key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

// ChatKeyMap defines key bindings for the chat view.
type ChatKeyMap struct {
	Cancel   key.Binding
	Back     key.Binding
	Esc      key.Binding
	Send     key.Binding
	Copy     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
}

var ChatKeys = ChatKeyMap{
	Cancel:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel/quit")),
	Back:     key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "back")),
	Esc:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "clear/back")),
	Send:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "send")),
	Copy:     key.NewBinding(key.WithKeys("alt+c"), key.WithHelp("Alt+C", "copy")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "scroll up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "scroll down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
}

func (k ChatKeyMap) HintsString() string {
	return JoinHints(
		"↑/↓ navigate",
		"PgUp/Dn page",
		Hint(k.Cancel, "stop"),
		Hint(k.Esc, "back"),
	)
}

// SearchKeyMap defines key bindings for the search list view.
type SearchKeyMap struct {
	Up         key.Binding
	Down       key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	Home       key.Binding
	End        key.Binding
	Enter      key.Binding
	Backspace  key.Binding
	Clear      key.Binding
	Provider   key.Binding
	Sort       key.Binding
	Tags       key.Binding
	DirectPull key.Binding
	Quit       key.Binding
}

var SearchKeys = SearchKeyMap{
	Up:         key.NewBinding(key.WithKeys("up"), key.WithHelp("↑/↓", "navigate")),
	Down:       key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "navigate")),
	PageUp:     key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown:   key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:       key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:        key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	Backspace:  key.NewBinding(key.WithKeys("backspace"), key.WithHelp("⌫", "delete")),
	Clear:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "clear")),
	Provider:   key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("^P", "provider")),
	Sort:       key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("^S", "sort")),
	Tags:       key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("^T", "tags")),
	DirectPull: key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("^D", "direct deploy")),
	Quit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
}

func (k SearchKeyMap) ListHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "select"),
		Hint(k.Clear, "back"),
		Hint(k.Provider, "provider"),
		Hint(k.Sort, "sort"),
		Hint(k.Tags, "tags"),
		Hint(k.DirectPull, "direct deploy"),
	)
}

func (k SearchKeyMap) ListHintsStringNoTags() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "select"),
		Hint(k.Clear, "back"),
		Hint(k.Provider, "provider"),
		Hint(k.Sort, "sort"),
		Hint(k.DirectPull, "direct deploy"),
	)
}

// SearchSubKeyMap defines key bindings shared across search sub-views
// (variant select, provider select, node select, picker overlay).
type SearchSubKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Enter    key.Binding
	Space    key.Binding
	Back     key.Binding
	Quit     key.Binding
}

var SearchSubKeys = SearchSubKeyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "select")),
	Space:    key.NewBinding(key.WithKeys("space"), key.WithHelp("Space", "toggle")),
	Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^C", "quit")),
}

func (k SearchKeyMap) DetailHintsString() string {
	return JoinHints(
		Hint(k.Up, "scroll"),
		Hint(key.NewBinding(key.WithKeys("p", "P"), key.WithHelp("P", "deploy")), "deploy"),
		Hint(key.NewBinding(key.WithKeys("m", "M"), key.WithHelp("M", "models")), "models"),
		Hint(k.Clear, "back"),
	)
}

// VkeysKeyMap defines key bindings for the virtual keys view.
type VkeysKeyMap struct {
	Up         key.Binding
	Down       key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	Home       key.Binding
	End        key.Binding
	Enter      key.Binding
	New        key.Binding
	Edit       key.Binding
	Delete     key.Binding
	Rotate     key.Binding
	Suspend    key.Binding
	JumpToTeam key.Binding
	ResetUsage key.Binding
	Refresh    key.Binding
	Copy       key.Binding
	SaveKey    key.Binding
	Dismiss    key.Binding
	Back       key.Binding
	Quit       key.Binding
}

var VkeysKeys = VkeysKeyMap{
	Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:     key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown:   key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:       key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:        key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	New:        key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("N", "new")),
	Edit:       key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("E", "edit")),
	Delete:     key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "delete")),
	Rotate:     key.NewBinding(key.WithKeys("o", "O"), key.WithHelp("O", "rotate")),
	Suspend:    key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "suspend")),
	JumpToTeam: key.NewBinding(key.WithKeys("t", "T"), key.WithHelp("T", "team")),
	ResetUsage: key.NewBinding(key.WithKeys("u", "U"), key.WithHelp("U", "reset usage")),
	Refresh:    key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Copy:       key.NewBinding(key.WithKeys("c", "C", "y", "Y"), key.WithHelp("C", "copy")),
	SaveKey:    key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "save to file")),
	Dismiss:    key.NewBinding(key.WithKeys("enter", "esc", "q"), key.WithHelp("Enter", "dismiss")),
	Back:       key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func (k VkeysKeyMap) ListHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "details"),
		Hint(k.New, "new"),
		Hint(k.Delete, "delete"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "back"),
	)
}

func (k VkeysKeyMap) DetailHintsString() string {
	return JoinHints(
		Hint(k.Up, "scroll"),
		Hint(k.Edit, "edit"),
		Hint(k.Rotate, "rotate"),
		Hint(k.Suspend, "suspend"),
		Hint(k.JumpToTeam, "go to team"),
		Hint(k.ResetUsage, "reset usage"),
		Hint(k.Delete, "delete"),
		Hint(k.Back, "back"),
	)
}

// RawKeyHintsString is the footer for the one-shot secret overlay. Only
// copy and an explicit dismiss are offered: the secret is unrecoverable
// once the overlay closes, so no other keystroke may close it.
func (k VkeysKeyMap) RawKeyHintsString() string {
	return JoinHints(
		Hint(k.Copy, "copy"),
		Hint(k.SaveKey, "save to file"),
		Hint(k.Dismiss, "dismiss"),
	)
}

// PricingKeyMap defines key bindings for the pricing-overrides view.
type PricingKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Enter    key.Binding
	New      key.Binding
	Edit     key.Binding
	Delete   key.Binding
	Unpriced key.Binding
	Refresh  key.Binding
	Back     key.Binding
	Quit     key.Binding
}

var PricingKeys = PricingKeyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "price it")),
	New:      key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("N", "new")),
	Edit:     key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("E", "edit")),
	Delete:   key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "delete")),
	Unpriced: key.NewBinding(key.WithKeys("u", "U"), key.WithHelp("U", "un-priced")),
	Refresh:  key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func (k PricingKeyMap) ListHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.New, "new"),
		Hint(k.Edit, "edit"),
		Hint(k.Delete, "delete"),
		Hint(k.Unpriced, "un-priced"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "back"),
	)
}

// UnpricedHintsString is the footer for the un-priced list, where Enter
// opens a prefilled override form for the model under the cursor.
func (k PricingKeyMap) UnpricedHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "price it"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Unpriced, "back to overrides"),
	)
}

// TeamsKeyMap defines key bindings for the teams view.
// Mirrors VkeysKeyMap minus Rotate (teams have no raw secret to rotate).
type TeamsKeyMap struct {
	Up         key.Binding
	Down       key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	Home       key.Binding
	End        key.Binding
	Enter      key.Binding
	New        key.Binding
	Edit       key.Binding
	Delete     key.Binding
	Suspend    key.Binding
	JumpToKey  key.Binding
	ResetUsage key.Binding
	Refresh    key.Binding
	Back       key.Binding
	Quit       key.Binding
}

var TeamsKeys = TeamsKeyMap{
	Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:     key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown:   key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:       key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:        key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	New:        key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("N", "new")),
	Edit:       key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("E", "edit")),
	Delete:     key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "delete")),
	Suspend:    key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "suspend")),
	JumpToKey:  key.NewBinding(key.WithKeys("v", "V"), key.WithHelp("V", "view key")),
	ResetUsage: key.NewBinding(key.WithKeys("u", "U"), key.WithHelp("U", "reset usage")),
	Refresh:    key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:       key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func (k TeamsKeyMap) ListHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "details"),
		Hint(k.New, "new"),
		Hint(k.Delete, "delete"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "back"),
	)
}

func (k TeamsKeyMap) DetailHintsString() string {
	return JoinHints(
		Hint(k.Up, "scroll"),
		Hint(k.Edit, "edit"),
		Hint(k.Suspend, "suspend"),
		Hint(k.JumpToKey, "view key"),
		Hint(k.ResetUsage, "reset usage"),
		Hint(k.Delete, "delete"),
		Hint(k.Back, "back"),
	)
}

// KvEditorKeyMap defines key bindings for the key-value editor list mode.
type KvEditorKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Enter    key.Binding
	Add      key.Binding
	Delete   key.Binding
	Validate key.Binding
	Save     key.Binding
	Back     key.Binding
	Quit     key.Binding
}

var KvEditorKeys = KvEditorKeyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "edit")),
	Add:      key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("A", "add")),
	Delete:   key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "delete")),
	Validate: key.NewBinding(key.WithKeys("v", "V"), key.WithHelp("V", "validate")),
	Save:     key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("S", "save")),
	Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func (k KvEditorKeyMap) HintsString(dirty bool) string {
	parts := []string{
		Hint(k.Up, "navigate"),
		Hint(k.Enter, "edit"),
		Hint(k.Add, "add"),
		Hint(k.Delete, "delete"),
		Hint(k.Validate, "validate"),
	}
	if dirty {
		parts = append(parts, Hint(k.Save, "save"))
	}
	parts = append(parts, Hint(k.Back, "back"))
	return JoinHints(parts...)
}

// KvOverlayKeyMap defines key bindings for the value editor overlay.
type KvOverlayKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	Left     key.Binding
	Right    key.Binding
	Confirm  key.Binding
	Cancel   key.Binding
	Tab      key.Binding
	ShiftTab key.Binding
}

var KvOverlayKeys = KvOverlayKeyMap{
	Up:       key.NewBinding(key.WithKeys("up"), key.WithHelp("↑/↓", "move field")),
	Down:     key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "move field")),
	Left:     key.NewBinding(key.WithKeys("left"), key.WithHelp("←/→", "change value")),
	Right:    key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "change value")),
	Confirm:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "confirm")),
	Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "cancel")),
	Tab:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "next field")),
	ShiftTab: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("Shift+Tab", "prev field")),
}

func (k KvOverlayKeyMap) EditHintsString() string {
	return JoinHints(
		Hint(k.Up, "select"),
		Hint(k.Confirm, "confirm"),
		Hint(k.Cancel, "cancel"),
	)
}

func (k KvOverlayKeyMap) AddHintsString() string {
	return JoinHints(
		Hint(k.Up, "move field"),
		Hint(k.Left, "change type"),
		Hint(k.Confirm, "confirm"),
		Hint(k.Cancel, "cancel"),
	)
}

// hint formats a single binding as "key desc".
func Hint(b key.Binding, desc string) string {
	return b.Help().Key + " " + desc
}

// joinHints joins formatted hint strings with double-space separators.
func JoinHints(parts ...string) string {
	return strings.Join(parts, "  ")
}

// DefaultConfirmKeyBindings returns the standard yes/no/cancel bindings
// used by confirmation dialogs across views.
var DefaultConfirmKeyBindings = ConfirmKeys{
	Yes:    key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("Y", "yes")),
	No:     key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("N", "no")),
	Cancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "cancel")),
}

// UpdateKeyMap drives the software-update view.
//
// Rollback is bound to B, not R: R is refresh in every other view, and
// the one key an operator presses out of habit must not be the one that
// moves the node back a version.
//
// A is install, which is what A does in the providers view too. C is
// copy or chat elsewhere, and this view has neither, so it is free for
// check — U was the alternative and is worse: in providers U *performs*
// an upgrade, which is precisely what check here does not do.
type UpdateKeyMap struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Check    key.Binding
	Apply    key.Binding
	Rollback key.Binding
	History  key.Binding
	Refresh  key.Binding
	Back     key.Binding
	Quit     key.Binding
}

var UpdateKeys = UpdateKeyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Check:    key.NewBinding(key.WithKeys("c", "C"), key.WithHelp("C", "check now")),
	Apply:    key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("A", "install")),
	Rollback: key.NewBinding(key.WithKeys("b", "B"), key.WithHelp("B", "roll back")),
	History:  key.NewBinding(key.WithKeys("h", "H"), key.WithHelp("H", "history")),
	Refresh:  key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

// StatusHintsString is the footer for the status body.
func (k UpdateKeyMap) StatusHintsString() string {
	return JoinHints(
		Hint(k.Check, "check now"),
		Hint(k.Apply, "install"),
		Hint(k.Rollback, "roll back"),
		Hint(k.History, "history"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "back"),
	)
}

// HistoryHintsString is the footer for the history list.
func (k UpdateKeyMap) HistoryHintsString() string {
	return JoinHints(
		Hint(k.Up, "navigate"),
		Hint(k.Refresh, "refresh"),
		Hint(k.Back, "status"),
	)
}
