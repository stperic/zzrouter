package metadata

// TrustedSource represents a trusted model repository source.
type TrustedSource struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Priority    int      `yaml:"priority" json:"priority"` // Lower number = higher priority
	Formats     []string `yaml:"formats" json:"formats"`   // Supported formats: gguf, onnx, etc.
	Enabled     bool     `yaml:"enabled" json:"enabled"`
}

// SourceRegistry manages trusted model sources.
type SourceRegistry struct {
	Sources []TrustedSource `yaml:"sources" json:"sources"`
}

// SmartPullStrategy determines the best download strategy.
type SmartPullStrategy int

const (
	// StrategySmartDefault tries pre-converted first, falls back to source.
	StrategySmartDefault SmartPullStrategy = iota

	// StrategyPreConvertedOnly only downloads pre-converted (fails if not found).
	StrategyPreConvertedOnly

	// StrategySourceOnly only downloads source (for security/trust).
	StrategySourceOnly

	// StrategyBoth downloads both source and pre-converted.
	StrategyBoth
)
