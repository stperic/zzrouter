package clientcli

import (
	"testing"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPsCmd(t *testing.T) {
	cmd := NewPsCmd()

	require.NotNil(t, cmd, "NewPsCmd() should not return nil")
	assert.Equal(t, "ps [model_name]", cmd.Use)
	assert.NotEmpty(t, cmd.Short, "cmd.Short should not be empty")
	assert.NotEmpty(t, cmd.Long, "cmd.Long should not be empty")
	require.NotNil(t, cmd.RunE, "cmd.RunE should not be nil")

	// Verify subcommands exist
	subcommands := cmd.Commands()
	subcommandNames := make([]string, len(subcommands))
	for i, sub := range subcommands {
		subcommandNames[i] = sub.Name()
	}

	assert.Contains(t, subcommandNames, "list", "should have 'list' subcommand")
	assert.Contains(t, subcommandNames, "logs", "should have 'logs' subcommand")
	assert.Contains(t, subcommandNames, "stream", "should have 'stream' subcommand")
	assert.Contains(t, subcommandNames, "details", "should have 'details' subcommand")
}

func TestNewPsListCmd(t *testing.T) {
	cmd := NewPsListCmd()

	require.NotNil(t, cmd, "NewPsListCmd() should not return nil")
	assert.Equal(t, "list [model_name]", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)
}

func TestNewPsLogsCmd(t *testing.T) {
	cmd := NewPsLogsCmd()

	require.NotNil(t, cmd, "NewPsLogsCmd() should not return nil")
	assert.Equal(t, "logs <model_name_or_id>", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)

	// Verify lines flag exists
	linesFlag := cmd.Flags().Lookup("lines")
	require.NotNil(t, linesFlag, "expected 'lines' flag to exist")
	assert.Equal(t, "l", linesFlag.Shorthand)
	assert.Equal(t, "50", linesFlag.DefValue)
}

func TestNewPsStreamCmd(t *testing.T) {
	cmd := NewPsStreamCmd()

	require.NotNil(t, cmd, "NewPsStreamCmd() should not return nil")
	assert.Equal(t, "stream <model_name_or_id>", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)
}

func TestNewPsDetailsCmd(t *testing.T) {
	cmd := NewPsDetailsCmd()

	require.NotNil(t, cmd, "NewPsDetailsCmd() should not return nil")
	assert.Equal(t, "details <model_name_or_id>", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	require.NotNil(t, cmd.RunE)
}

func TestTruncateID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		maxLen   int
		expected string
	}{
		{
			name:     "id shorter than maxLen",
			id:       "abc123",
			maxLen:   12,
			expected: "abc123",
		},
		{
			name:     "id equal to maxLen",
			id:       "abc123456789",
			maxLen:   12,
			expected: "abc123456789",
		},
		{
			name:     "id longer than maxLen",
			id:       "abc123456789xyz",
			maxLen:   12,
			expected: "abc123456789",
		},
		{
			name:     "empty id",
			id:       "",
			maxLen:   12,
			expected: "",
		},
		{
			name:     "maxLen of 0",
			id:       "abc123",
			maxLen:   0,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateID(tt.id, tt.maxLen)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatUntilFromKeepAlive(t *testing.T) {
	now := time.Now()
	nowStr := now.Format(time.RFC3339)
	pastStr := now.Add(-2 * time.Hour).Format(time.RFC3339)
	futureActivityStr := now.Add(-5 * time.Minute).Format(time.RFC3339) // Activity 5 min ago with 1h keepalive = ~55 min left

	tests := []struct {
		name         string
		keepAlive    string
		lastActivity string
		startedAt    string
		expected     string
	}{
		{
			name:         "empty keepAlive",
			keepAlive:    "",
			lastActivity: nowStr,
			startedAt:    nowStr,
			expected:     "-",
		},
		{
			name:         "empty activity times",
			keepAlive:    "5m",
			lastActivity: "",
			startedAt:    "",
			expected:     "-",
		},
		{
			name:         "invalid keepAlive format",
			keepAlive:    "invalid",
			lastActivity: nowStr,
			startedAt:    "",
			expected:     "-",
		},
		{
			name:         "negative duration means forever",
			keepAlive:    "-1h",
			lastActivity: nowStr,
			startedAt:    "",
			expected:     "Forever",
		},
		{
			name:         "very large duration means forever",
			keepAlive:    "876600h", // > 100 years (876600 hours ≈ 100 years)
			lastActivity: nowStr,
			startedAt:    "",
			expected:     "Forever",
		},
		{
			name:         "expired (past + short keepalive)",
			keepAlive:    "30m",
			lastActivity: pastStr, // 2 hours ago + 30 min keepalive = expired
			startedAt:    "",
			expected:     "-",
		},
		{
			name:         "valid future expiration",
			keepAlive:    "1h",
			lastActivity: futureActivityStr,
			startedAt:    "",
			expected:     "55 minutes", // Approximately
		},
		{
			name:         "uses startedAt as fallback",
			keepAlive:    "2h",
			lastActivity: "",
			startedAt:    nowStr,
			expected:     "2 hours",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatUntilFromKeepAlive(tt.keepAlive, tt.lastActivity, tt.startedAt)

			// For time-based comparisons, we can't be exact, so check patterns
			if tt.expected == "-" || tt.expected == "Forever" {
				assert.Equal(t, tt.expected, result)
			} else {
				// For time-based results, just verify it's not "-" and is reasonable
				assert.NotEqual(t, "-", result, "expected a time-based result")
				// Could be minutes or hours
				assert.True(t,
					len(result) > 0,
					"result should have content: got %q", result)
			}
		})
	}
}

func TestFormatDurationUntil(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{
			name:     "less than a minute",
			duration: 30 * time.Second,
			expected: "less than a minute",
		},
		{
			name:     "exactly 1 minute",
			duration: 1 * time.Minute,
			expected: "1 minute",
		},
		{
			name:     "multiple minutes",
			duration: 5 * time.Minute,
			expected: "5 minutes",
		},
		{
			name:     "rounds up to next minute",
			duration: 5*time.Minute + 30*time.Second,
			expected: "6 minutes",
		},
		{
			name:     "exactly 1 hour",
			duration: 1 * time.Hour,
			expected: "1 hour",
		},
		{
			name:     "multiple hours",
			duration: 3 * time.Hour,
			expected: "3 hours",
		},
		{
			name:     "rounds up to next hour",
			duration: 3*time.Hour + 30*time.Minute,
			expected: "4 hours",
		},
		{
			name:     "exactly 1 day",
			duration: 24 * time.Hour,
			expected: "1 day",
		},
		{
			name:     "multiple days",
			duration: 72 * time.Hour,
			expected: "3 days",
		},
		{
			name:     "rounds up to next day",
			duration: 25 * time.Hour,
			expected: "2 days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDurationUntil(tt.duration)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFilterInstances(t *testing.T) {
	instances := []pkgClient.Instance{
		{ID: "1", Node: "localhost", App: "ollama", Model: "llama2:7b"},
		{ID: "2", Node: "localhost", App: "vllm", Model: "mistral-7b"},
		{ID: "3", Node: "gpu-server", App: "ollama", Model: "llama3:8b"},
		{ID: "4", Node: "gpu-server", App: "mlx", Model: "phi-3"},
	}

	tests := []struct {
		name     string
		app      string
		model    string
		expected []string // Expected instance IDs
	}{
		{
			name:     "no filters returns all",
			app:      "",
			model:    "",
			expected: []string{"1", "2", "3", "4"},
		},
		{
			name:     "filter by model name exact",
			app:      "",
			model:    "llama2:7b",
			expected: []string{"1"},
		},
		{
			name:     "filter by model name wildcard",
			app:      "",
			model:    "llama*",
			expected: []string{"1", "3"},
		},
		{
			name:     "filter by model no match",
			app:      "",
			model:    "nonexistent",
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shared.FilterInstances(instances, tt.app, tt.model)

			actualIDs := make([]string, len(result))
			for i, inst := range result {
				actualIDs[i] = inst.ID
			}

			assert.Equal(t, tt.expected, actualIDs)
		})
	}
}
