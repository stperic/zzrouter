package config

// ParameterDeclaration is an operator's optional shape fields in config.yaml.
type ParameterDeclaration struct {
	Type        *string   `yaml:"type,omitempty" json:"type,omitempty"`
	Min         *float64  `yaml:"min,omitempty" json:"min,omitempty"`
	Max         *float64  `yaml:"max,omitempty" json:"max,omitempty"`
	Enum        *[]string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Description *string   `yaml:"description,omitempty" json:"description,omitempty"`
	Pass        *string   `yaml:"pass,omitempty" json:"pass,omitempty"`
	Auto        *string   `yaml:"auto,omitempty" json:"auto,omitempty"`
	Negatable   *bool     `yaml:"negatable,omitempty" json:"negatable,omitempty"`
}
