package search

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProvider_String(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		expected string
	}{
		{"HuggingFace", ProviderHuggingFace, "huggingface"},
		{"Ollama", ProviderOllama, "ollama"},
		{"All", ProviderAll, "all"},
		{"Invalid", Provider("invalid"), "invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.provider.String()
			if result != tt.expected {
				t.Errorf("Provider.String() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestProvider_IsValid(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		expected bool
	}{
		{"HuggingFace valid", ProviderHuggingFace, true},
		{"Ollama valid", ProviderOllama, true},
		{"All valid", ProviderAll, true},
		{"Invalid provider", Provider("invalid"), false},
		{"Empty provider", Provider(""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.provider.IsValid()
			if result != tt.expected {
				t.Errorf("Provider.IsValid() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseProvider(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    Provider
		expectError bool
	}{
		{"HuggingFace full", "huggingface", ProviderHuggingFace, false},
		{"HuggingFace short", "hf", ProviderHuggingFace, false},
		{"HuggingFace case", "HUGGINGFACE", ProviderHuggingFace, false},
		{"Ollama", "ollama", ProviderOllama, false},
		{"Ollama case", "OLLAMA", ProviderOllama, false},
		{"All", "all", ProviderAll, false},
		{"Empty (all)", "", ProviderAll, false},
		{"Invalid", "invalid", "", true},
		{"Unknown", "unknown", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseProvider(tt.input)

			if tt.expectError {
				if err == nil {
					t.Errorf("ParseProvider(%q) expected error, got nil", tt.input)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseProvider(%q) unexpected error: %v", tt.input, err)
				return
			}

			if result != tt.expected {
				t.Errorf("ParseProvider(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSearchParams_Validate(t *testing.T) {
	tests := []struct {
		name    string
		params  SearchParams
		wantErr error // nil = expect no error
	}{
		{
			name: "Valid params",
			params: SearchParams{
				Provider: ProviderHuggingFace,
				Pagination: PaginationParams{
					Limit:  20,
					Offset: 0,
				},
			},
		},
		{
			name: "Invalid provider",
			params: SearchParams{
				Provider: Provider("invalid"),
				Pagination: PaginationParams{
					Limit:  20,
					Offset: 0,
				},
			},
			wantErr: ErrInvalidProvider,
		},
		{
			name: "Negative limit",
			params: SearchParams{
				Provider: ProviderHuggingFace,
				Pagination: PaginationParams{
					Limit:  -1,
					Offset: 0,
				},
			},
			wantErr: ErrLimitNegative,
		},
		{
			name: "Limit too large",
			params: SearchParams{
				Provider: ProviderHuggingFace,
				Pagination: PaginationParams{
					Limit:  2000,
					Offset: 0,
				},
			},
			wantErr: ErrLimitExceedsMax,
		},
		{
			name: "Negative offset",
			params: SearchParams{
				Provider: ProviderHuggingFace,
				Pagination: PaginationParams{
					Limit:  20,
					Offset: -1,
				},
			},
			wantErr: ErrOffsetNegative,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.params.Validate()

			if tt.wantErr != nil {
				require.Error(t, err)
				require.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestPaginationParams_Validate(t *testing.T) {
	tests := []struct {
		name       string
		pagination PaginationParams
		wantErr    error // nil = expect no error
	}{
		{
			name: "Valid pagination",
			pagination: PaginationParams{
				Limit:  20,
				Offset: 0,
			},
		},
		{
			name: "Zero limit (valid)",
			pagination: PaginationParams{
				Limit:  0,
				Offset: 0,
			},
		},
		{
			name: "Negative limit",
			pagination: PaginationParams{
				Limit:  -1,
				Offset: 0,
			},
			wantErr: ErrLimitNegative,
		},
		{
			name: "Limit too large",
			pagination: PaginationParams{
				Limit:  2000,
				Offset: 0,
			},
			wantErr: ErrLimitExceedsMax,
		},
		{
			name: "Negative offset",
			pagination: PaginationParams{
				Limit:  20,
				Offset: -1,
			},
			wantErr: ErrOffsetNegative,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pagination.Validate()

			if tt.wantErr != nil {
				require.Error(t, err)
				require.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
}
