package update

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewScheduler_DefaultSource(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{}, clock.System())

	assert.Equal(t, DefaultOwner, s.checker.owner)
	assert.Equal(t, DefaultRepo, s.checker.repo)
	assert.Equal(t, GitHubAPIBase, s.checker.apiBaseURL)
	assert.Empty(t, s.downloader.allowedHosts, "an empty allowlist means the github.com defaults apply")
	assert.Empty(t, s.GetStatus().Source, "the public feed is not worth announcing")
}

// TestNewScheduler_SourceOverride pins the whole override reaching the
// pieces that act on it. A source that only reached the checker would
// find the mock release and then refuse to download it.
func TestNewScheduler_SourceOverride(t *testing.T) {
	cfg := &config.UpdateConfig{
		Source: &config.UpdateSourceConfig{
			APIBaseURL:   "https://127.0.0.1:8443",
			Owner:        "acme",
			Repo:         "widget",
			AllowedHosts: []string{"127.0.0.1:8443"},
		},
	}

	s := NewScheduler(cfg, clock.System())

	assert.Equal(t, "acme", s.checker.owner)
	assert.Equal(t, "widget", s.checker.repo)
	assert.Equal(t, "https://127.0.0.1:8443", s.checker.apiBaseURL)
	assert.Equal(t, []string{"127.0.0.1:8443"}, s.downloader.allowedHosts)
	assert.Equal(t, "acme/widget at https://127.0.0.1:8443", s.GetStatus().Source)
}

func TestUpdateSourceConfig_Validate(t *testing.T) {
	valid := func() *config.UpdateSourceConfig {
		return &config.UpdateSourceConfig{
			APIBaseURL:   "https://127.0.0.1:8443",
			Owner:        "acme",
			Repo:         "widget",
			AllowedHosts: []string{"127.0.0.1:8443"},
		}
	}

	require.NoError(t, (&config.UpdateConfig{Source: valid()}).Validate())

	// Plaintext is fine to a loopback literal, which is what the local
	// rehearsal harness uses.
	loopback := valid()
	loopback.APIBaseURL = "http://127.0.0.1:8099"
	require.NoError(t, (&config.UpdateConfig{Source: loopback}).Validate())

	tests := map[string]func(*config.UpdateSourceConfig){
		"missing api_base_url":  func(s *config.UpdateSourceConfig) { s.APIBaseURL = "" },
		"missing owner":         func(s *config.UpdateSourceConfig) { s.Owner = "" },
		"missing repo":          func(s *config.UpdateSourceConfig) { s.Repo = "" },
		"missing allowed_hosts": func(s *config.UpdateSourceConfig) { s.AllowedHosts = nil },
		"non-http scheme":       func(s *config.UpdateSourceConfig) { s.APIBaseURL = "ftp://example.test" },
		"no host":               func(s *config.UpdateSourceConfig) { s.APIBaseURL = "https://" },

		// The metadata hop picks the download URL and decides whether a
		// signature is even expected, so plaintext to a remote host
		// hands the binary choice to anyone on the path.
		"plaintext to a remote host": func(s *config.UpdateSourceConfig) {
			s.APIBaseURL = "http://releases.corp.internal"
		},
		"plaintext to a name that merely looks local": func(s *config.UpdateSourceConfig) {
			s.APIBaseURL = "http://localhost:8099"
		},
	}

	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			source := valid()
			break_(source)
			assert.Error(t, (&config.UpdateConfig{Source: source}).Validate())
		})
	}
}
