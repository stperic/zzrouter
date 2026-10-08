package servercli

// Provider represents a provider option
type Provider struct {
	Name        string
	Description string
	Mode        string // "on-demand", "service", "external", "cloud"
	Enabled     bool
	Selected    bool
	Warning     string // Platform-specific warnings
}
