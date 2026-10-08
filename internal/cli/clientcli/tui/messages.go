package tui

import logsclient "github.com/stperic/zzrouter/internal/client/logs"

// TuiBackMsg signals the parent TUI to navigate back to the menu.
type TuiBackMsg struct{}

// SwitchToModelsMsg signals the parent TUI to switch to the models view.
type SwitchToModelsMsg struct{}

// LaunchChatMsg signals the parent TUI to open the chat view with the given model.
type LaunchChatMsg struct {
	Model string
	Node  string
}

// OpenParamsEditorMsg tells the parent TUI to open the params view.
type OpenParamsEditorMsg struct {
	Provider string
	Model    string
}

// LaunchRunLogsMsg asks the root tuiModel to switch to the run logs view.
type LaunchRunLogsMsg struct {
	Filter logsclient.RunFilter
}

// QsLaunchViewMsg tells tuiModel to navigate to a view and return afterwards.
type QsLaunchViewMsg struct {
	View View
	Step int
}

// QsStepDoneMsg marks a wizard step as complete (sent by tuiModel on return).
type QsStepDoneMsg struct {
	Step    int
	Summary string
}

// QsEnterMenuMsg transitions from quickstart to the main TUI menu.
type QsEnterMenuMsg struct{}
