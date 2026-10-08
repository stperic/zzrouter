package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stretchr/testify/require"
)

func TestFilterCandidatesByProvider(t *testing.T) {
	tests := []struct {
		name      string
		deps      []fallback.Candidate
		provider  string
		wantNames []string
		wantLen   int
	}{
		{
			name:      "empty input returns empty",
			deps:      nil,
			provider:  "ollama",
			wantNames: nil,
			wantLen:   0,
		},
		{
			name: "single matching candidate",
			deps: []fallback.Candidate{
				{Name: "a", App: "ollama"},
			},
			provider:  "ollama",
			wantNames: []string{"a"},
			wantLen:   1,
		},
		{
			name: "filters out non-matching provider",
			deps: []fallback.Candidate{
				{Name: "a", App: "ollama"},
				{Name: "b", App: "vllm"},
				{Name: "c", App: "ollama"},
			},
			provider:  "ollama",
			wantNames: []string{"a", "c"},
			wantLen:   2,
		},
		{
			name: "preserves ordering of matches",
			deps: []fallback.Candidate{
				{Name: "first", App: "ollama"},
				{Name: "skip", App: "vllm"},
				{Name: "second", App: "ollama"},
				{Name: "third", App: "ollama"},
			},
			provider:  "ollama",
			wantNames: []string{"first", "second", "third"},
			wantLen:   3,
		},
		{
			name: "no matches returns empty slice",
			deps: []fallback.Candidate{
				{Name: "a", App: "vllm"},
				{Name: "b", App: "mlx"},
			},
			provider:  "ollama",
			wantNames: []string{},
			wantLen:   0,
		},
		{
			name: "case-sensitive match",
			deps: []fallback.Candidate{
				{Name: "a", App: "Ollama"},
				{Name: "b", App: "ollama"},
			},
			provider:  "ollama",
			wantNames: []string{"b"},
			wantLen:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterCandidatesByProvider(tt.deps, tt.provider)
			require.Len(t, got, tt.wantLen)
			names := make([]string, 0, len(got))
			for _, d := range got {
				names = append(names, d.Name)
			}
			if tt.wantLen > 0 {
				require.Equal(t, tt.wantNames, names)
			}
		})
	}
}

// TestFilterCandidatesByProvider_DoesNotMutate verifies the input slice is
// left untouched — strategies and other callers rely on this contract.
func TestFilterCandidatesByProvider_DoesNotMutate(t *testing.T) {
	orig := []fallback.Candidate{
		{Name: "a", App: "ollama"},
		{Name: "b", App: "vllm"},
	}
	snapshot := make([]fallback.Candidate, len(orig))
	copy(snapshot, orig)

	_ = filterCandidatesByProvider(orig, "ollama")

	require.Equal(t, snapshot, orig, "input slice must not be mutated")
}
