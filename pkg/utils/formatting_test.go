package utils

import (
	"testing"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want string
	}{
		{
			name: "zero size",
			size: 0,
			want: "N/A",
		},
		{
			name: "bytes",
			size: 500,
			want: "500 B",
		},
		{
			name: "kilobytes",
			size: 1500,
			want: "1.5 KB",
		},
		{
			name: "megabytes",
			size: 1500000,
			want: "1.5 MB",
		},
		{
			name: "gigabytes",
			size: 1500000000,
			want: "1.5 GB",
		},
		{
			name: "terabytes",
			size: 1500000000000,
			want: "1.5 TB",
		},
		{
			name: "petabytes",
			size: 1500000000000000,
			want: "1.5 PB",
		},
		{
			name: "exabytes",
			size: 1500000000000000000,
			want: "1.5 EB",
		},
		{
			name: "exactly 1 KB",
			size: 1000,
			want: "1.0 KB",
		},
		{
			name: "exactly 1 MB",
			size: 1000000,
			want: "1.0 MB",
		},
		{
			name: "exactly 1 GB",
			size: 1000000000,
			want: "1.0 GB",
		},
		{
			name: "small bytes",
			size: 1,
			want: "1 B",
		},
		{
			name: "999 bytes",
			size: 999,
			want: "999 B",
		},
		{
			name: "complex decimal",
			size: 1234567,
			want: "1.2 MB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatSize(tt.size)
			if got != tt.want {
				t.Errorf("FormatSize(%d) = %v, want %v", tt.size, got, tt.want)
			}
		})
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		text    string
		want    bool
	}{
		// Empty cases
		{
			name:    "both empty",
			pattern: "",
			text:    "",
			want:    true,
		},
		{
			name:    "empty pattern, non-empty text",
			pattern: "",
			text:    "hello",
			want:    false,
		},
		{
			name:    "wildcard matches anything",
			pattern: "*",
			text:    "anything",
			want:    true,
		},
		{
			name:    "wildcard matches empty",
			pattern: "*",
			text:    "",
			want:    true,
		},

		// Case insensitive
		{
			name:    "case insensitive exact match",
			pattern: "HELLO",
			text:    "hello",
			want:    true,
		},
		{
			name:    "case insensitive substring",
			pattern: "LLO",
			text:    "Hello World",
			want:    true,
		},

		// No wildcards - substring matching
		{
			name:    "substring match at beginning",
			pattern: "hello",
			text:    "hello world",
			want:    true,
		},
		{
			name:    "substring match in middle",
			pattern: "world",
			text:    "hello world today",
			want:    true,
		},
		{
			name:    "substring match at end",
			pattern: "end",
			text:    "this is the end",
			want:    true,
		},
		{
			name:    "no substring match",
			pattern: "xyz",
			text:    "hello world",
			want:    false,
		},

		// * wildcard (zero or more characters)
		{
			name:    "star at beginning",
			pattern: "*world",
			text:    "hello world",
			want:    true,
		},
		{
			name:    "star at end",
			pattern: "hello*",
			text:    "hello world",
			want:    true,
		},
		{
			name:    "star in middle",
			pattern: "hello*world",
			text:    "hello beautiful world",
			want:    true,
		},
		{
			name:    "multiple stars",
			pattern: "*hello*world*",
			text:    "well hello there world today",
			want:    true,
		},
		{
			name:    "star matches empty",
			pattern: "hello*world",
			text:    "helloworld",
			want:    true,
		},
		{
			name:    "star no match",
			pattern: "hello*xyz",
			text:    "hello world",
			want:    false,
		},

		// ? wildcard (exactly one character)
		{
			name:    "question mark single char",
			pattern: "h?llo",
			text:    "hello",
			want:    true,
		},
		{
			name:    "question mark single char alt",
			pattern: "h?llo",
			text:    "hallo",
			want:    true,
		},
		{
			name:    "question mark no match - too short",
			pattern: "h?llo",
			text:    "hllo",
			want:    false,
		},
		{
			name:    "question mark no match - too long",
			pattern: "h?llo",
			text:    "heello",
			want:    false,
		},
		{
			name:    "multiple question marks",
			pattern: "h??lo",
			text:    "hello",
			want:    true,
		},

		// Mixed wildcards
		{
			name:    "star and question mark",
			pattern: "h?llo*world",
			text:    "hello beautiful world",
			want:    true,
		},
		{
			name:    "complex pattern",
			pattern: "*h?llo*w?rld*",
			text:    "say hello to the world today",
			want:    true,
		},

		// Model name patterns (real-world examples)
		{
			name:    "model name wildcard",
			pattern: "llama*",
			text:    "llama3.1:8b",
			want:    true,
		},
		{
			name:    "model name with version",
			pattern: "llama3.*",
			text:    "llama3.1:8b",
			want:    true,
		},
		{
			name:    "model family search",
			pattern: "*llama*",
			text:    "meta-llama/Llama-3-8B",
			want:    true,
		},
		{
			name:    "model size pattern",
			pattern: "*8b*",
			text:    "llama3.1:8b",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchPattern(tt.pattern, tt.text)
			if got != tt.want {
				t.Errorf("MatchPattern(%q, %q) = %v, want %v", tt.pattern, tt.text, got, tt.want)
			}
		})
	}
}

func TestFormatModelName(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		want      string
	}{
		{
			name:      "empty string",
			modelName: "",
			want:      "",
		},
		{
			name:      "simple name without slash",
			modelName: "llama3",
			want:      "llama3",
		},
		{
			name:      "name with single slash",
			modelName: "Qwen/Qwen3-0.6B",
			want:      "Qwen3-0.6B",
		},
		{
			name:      "name with multiple slashes",
			modelName: "lmstudio-community/Phi-4-mini-reasoning-MLX-4bit",
			want:      "Phi-4-mini-reasoning-MLX-4bit",
		},
		{
			name:      "deep path",
			modelName: "org/team/project/model-name",
			want:      "model-name",
		},
		{
			name:      "slash at end",
			modelName: "model-name/",
			want:      "",
		},
		{
			name:      "only slash",
			modelName: "/",
			want:      "",
		},
		{
			name:      "huggingface style",
			modelName: "meta-llama/Llama-3-8B",
			want:      "Llama-3-8B",
		},
		{
			name:      "ollama library style",
			modelName: "library/llama3.1:8b",
			want:      "llama3.1:8b",
		},
		{
			name:      "with colon (tag)",
			modelName: "mistral/mistral-7b:latest",
			want:      "mistral-7b:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatModelName(tt.modelName)
			if got != tt.want {
				t.Errorf("FormatModelName(%q) = %v, want %v", tt.modelName, got, tt.want)
			}
		})
	}
}

// Benchmark tests
func BenchmarkFormatSize(b *testing.B) {
	sizes := []int64{0, 500, 1500, 1500000, 1500000000}
	for i := 0; i < b.N; i++ {
		for _, size := range sizes {
			FormatSize(size)
		}
	}
}

func BenchmarkMatchPattern(b *testing.B) {
	patterns := []struct {
		pattern string
		text    string
	}{
		{"hello", "hello world"},
		{"*world", "hello world"},
		{"h?llo*", "hello there"},
		{"*llama*", "meta-llama/Llama-3-8B"},
	}

	for i := 0; i < b.N; i++ {
		for _, p := range patterns {
			MatchPattern(p.pattern, p.text)
		}
	}
}

func BenchmarkFormatModelName(b *testing.B) {
	names := []string{
		"llama3",
		"Qwen/Qwen3-0.6B",
		"lmstudio-community/Phi-4-mini-reasoning-MLX-4bit",
		"org/team/project/model-name",
	}

	for i := 0; i < b.N; i++ {
		for _, name := range names {
			FormatModelName(name)
		}
	}
}
