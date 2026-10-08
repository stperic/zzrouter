package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionSourceValidate(t *testing.T) {
	tests := []struct {
		name    string
		src     *VersionSource
		wantErr string
	}{
		{
			name: "github release",
			src:  &VersionSource{Type: VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", Compare: CompareBuildNumber},
		},
		{
			name: "github release defaults to opaque",
			src:  &VersionSource{Type: VersionSourceGitHubRelease, Repo: "ollama/ollama"},
		},
		{
			name: "pypi",
			src:  &VersionSource{Type: VersionSourcePyPI, Package: "mlx-lm", Compare: ComparePEP440},
		},
		{
			// Semver silently drops .postN and 4-segment identifiers, and
			// dropping the newest one reads as "up to date".
			name:    "pypi rejects semver",
			src:     &VersionSource{Type: VersionSourcePyPI, Package: "vllm", Compare: CompareSemver},
			wantErr: "requires compare: pep440",
		},
		{
			name:    "unknown type",
			src:     &VersionSource{Type: "gitlab", Repo: "a/b"},
			wantErr: "unknown type",
		},
		{
			name:    "unknown compare",
			src:     &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b", Compare: "chronological"},
			wantErr: "unknown compare",
		},
		{
			name:    "repo missing slash",
			src:     &VersionSource{Type: VersionSourceGitHubRelease, Repo: "llama.cpp"},
			wantErr: `must be "owner/name"`,
		},
		{
			name:    "package on a github source",
			src:     &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b", Package: "vllm"},
			wantErr: "package is not valid",
		},
		{
			name:    "repo on a pypi source",
			src:     &VersionSource{Type: VersionSourcePyPI, Package: "vllm", Repo: "a/b", Compare: ComparePEP440},
			wantErr: "repo is not valid",
		},
		{
			// The index hands back an unordered list, so there is nothing to
			// pick a latest from without an ordering.
			name:    "pypi without an ordering",
			src:     &VersionSource{Type: VersionSourcePyPI, Package: "vllm"},
			wantErr: "requires compare: pep440",
		},
		{
			name:    "nil source",
			src:     nil,
			wantErr: "nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.src.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// A crafted identifier must not be able to walk out of the URL template and
// reach a different path or host.
func TestVersionSourceRejectsPathEscape(t *testing.T) {
	escapes := []string{
		"..",
		"../..",
		"owner/..",
		"../evil",
		"owner/na me",
		"owner/name?x=1",
		"owner/name#frag",
		"owner//name",
		"owner/name/extra",
		"evil.com/owner/name",
		".hidden/name",
		"owner/.",
	}
	for _, repo := range escapes {
		t.Run(repo, func(t *testing.T) {
			src := &VersionSource{Type: VersionSourceGitHubRelease, Repo: repo}
			assert.Error(t, src.Validate(), "repo %q must be rejected", repo)
		})
	}

	for _, pkg := range []string{"..", "../evil", "vllm/../x", "vllm?x", ".vllm", "vllm."} {
		t.Run("pypi/"+pkg, func(t *testing.T) {
			src := &VersionSource{Type: VersionSourcePyPI, Package: pkg, Compare: ComparePEP440}
			assert.Error(t, src.Validate(), "package %q must be rejected", pkg)
		})
	}
}

func TestVersionSourceNormalize(t *testing.T) {
	llama := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}
	assert.Equal(t, "10453", llama.Normalize("b10453"))
	assert.Equal(t, "10453", llama.Normalize("  b10453  "))
	// Already normalized input stays put rather than losing a character.
	assert.Equal(t, "10453", llama.Normalize("10453"))

	ollama := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "ollama/ollama", StripPrefix: "v"}
	assert.Equal(t, "0.32.14", ollama.Normalize("v0.32.14"))

	var nilSrc *VersionSource
	assert.Equal(t, "1.0.0", nilSrc.Normalize("1.0.0"))
}

// Tag and Normalize together are what let a caller pass either field the
// versions endpoint reports ("latest" or "latest_tag") without building a
// 404 download URL.
func TestVersionSourceTag(t *testing.T) {
	llama := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "ggml-org/llama.cpp", StripPrefix: "b"}
	assert.Equal(t, "b10453", llama.Tag("10453"), "the normalized form gains the prefix")
	assert.Equal(t, "b10453", llama.Tag("b10453"), "the tag itself is left alone")
	assert.Equal(t, "b10453", llama.Tag("  10453  "))

	// Round-trip in both directions, which is the property callers rely on.
	assert.Equal(t, "b10453", llama.Tag(llama.Normalize("b10453")))
	assert.Equal(t, "10453", llama.Normalize(llama.Tag("10453")))

	// A value that is not the normalized form is passed through untouched:
	// prefixing it would corrupt an identifier the caller meant literally.
	assert.Equal(t, "v1.0.0", llama.Tag("v1.0.0"))
	assert.Equal(t, "master", llama.Tag("master"))
	assert.Empty(t, llama.Tag(""))

	noPrefix := &VersionSource{Type: VersionSourcePyPI, Package: "vllm", Compare: ComparePEP440}
	assert.Equal(t, "0.27.1", noPrefix.Tag("0.27.1"))

	var nilSrc *VersionSource
	assert.Equal(t, "1.0.0", nilSrc.Tag("1.0.0"))
}

func TestVersionSourceComparatorDefault(t *testing.T) {
	var nilSrc *VersionSource
	assert.Equal(t, CompareOpaque, nilSrc.Comparator())
	assert.Equal(t, CompareOpaque, (&VersionSource{}).Comparator())
	assert.Equal(t, CompareSemver, (&VersionSource{Compare: CompareSemver}).Comparator())
}

// VersionSource has to appear at every site a provider field passes through,
// and the conversion pair is where a missed one goes unnoticed: the field
// survives a write, then vanishes on the next load.
func TestVersionSourceSurvivesProviderRoundTrip(t *testing.T) {
	kinds := []struct {
		name string
		cfg  ServiceConfig
	}{
		{
			name: "on-demand",
			cfg: ServiceConfig{
				Protocol: ProtocolOpenAI,
				Mode:     "on-demand",
				VersionSource: &VersionSource{
					Type:        VersionSourceGitHubRelease,
					Repo:        "ggml-org/llama.cpp",
					StripPrefix: "b",
					Compare:     CompareBuildNumber,
				},
				Runtime: &AppRuntimeConfig{
					BasePort:  8080,
					PortRange: []int{8080, 8089},
					Execution: ExecutionConfig{Type: "cli", Command: "llama-server"},
				},
				Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			},
		},
		{
			name: "external",
			cfg: ServiceConfig{
				Protocol: ProtocolOllama,
				Mode:     "external",
				VersionSource: &VersionSource{
					Type:        VersionSourceGitHubRelease,
					Repo:        "ollama/ollama",
					StripPrefix: "v",
					Compare:     CompareSemver,
				},
				Runtime:      &AppRuntimeConfig{Endpoint: "http://127.0.0.1:11434"},
				Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
			},
		},
	}

	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			cfg := &AppsConfig{}
			require.NoError(t, cfg.AddApp("engine", k.cfg))

			got, ok := cfg.LookupApp("engine")
			require.True(t, ok)
			require.NotNil(t, got.VersionSource, "version_source was dropped in conversion")

			assert.Equal(t, k.cfg.VersionSource.Type, got.VersionSource.Type)
			assert.Equal(t, k.cfg.VersionSource.Repo, got.VersionSource.Repo)
			assert.Equal(t, k.cfg.VersionSource.StripPrefix, got.VersionSource.StripPrefix)
			assert.Equal(t, k.cfg.VersionSource.Compare, got.VersionSource.Compare)
			require.NoError(t, got.VersionSource.Validate())
		})
	}
}

func TestVersionSourceHomebrewValidate(t *testing.T) {
	ok := &VersionSource{Type: VersionSourceHomebrew, Formula: "ollama", Compare: CompareSemver}
	assert.NoError(t, ok.Validate())

	// A formula is what a homebrew source resolves; without it there is
	// nothing to ask about.
	bad := &VersionSource{Type: VersionSourceHomebrew, Compare: CompareSemver}
	assert.Error(t, bad.Validate())

	// Identifiers from the other sources are not silently ignored: carrying
	// one means the config author expected a different lookup than the one
	// this source performs.
	for _, src := range []*VersionSource{
		{Type: VersionSourceHomebrew, Formula: "ollama", Repo: "ollama/ollama"},
		{Type: VersionSourceHomebrew, Formula: "ollama", Package: "ollama"},
		{Type: VersionSourceGitHubRelease, Repo: "a/b", Formula: "ollama"},
	} {
		assert.Error(t, src.Validate(), "%+v must be rejected", src)
	}

	// The charset is what keeps an identifier from walking out of the URL
	// template's path.
	for _, formula := range []string{"../etc/passwd", "ollama/x", ".hidden", ""} {
		src := &VersionSource{Type: VersionSourceHomebrew, Formula: formula}
		assert.Error(t, src.Validate(), "formula %q must be rejected", formula)
	}
}

// A provider can be installed by different means per platform, and each
// installer has its own ceiling. Reporting one answer for both platforms is
// wrong for one of them, in the direction that offers an impossible upgrade.
func TestVersionSourcePlatformOverrides(t *testing.T) {
	brew := &VersionSource{Type: VersionSourceHomebrew, Formula: "ollama", Compare: CompareSemver}
	base := &VersionSource{
		Type: VersionSourceGitHubRelease, Repo: "ollama/ollama",
		StripPrefix: "v", Compare: CompareSemver,
		Platforms: map[string]*VersionSource{"darwin": brew},
	}
	require.NoError(t, base.Validate())

	assert.Same(t, brew, base.ForOS("darwin"))
	assert.Same(t, base, base.ForOS("linux"), "no override means the base source")
	// An unknown platform gets the base rather than a guess: comparing
	// against the wrong ceiling is worse than comparing against the project's.
	assert.Same(t, base, base.ForOS(""))

	var nilSrc *VersionSource
	assert.Nil(t, nilSrc.ForOS("darwin"))
}

func TestVersionSourcePlatformOverridesRejected(t *testing.T) {
	brew := func() *VersionSource {
		return &VersionSource{Type: VersionSourceHomebrew, Formula: "ollama", Compare: CompareSemver}
	}

	// A typo'd OS would be an override that silently never applies.
	typo := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b",
		Platforms: map[string]*VersionSource{"macos": brew()}}
	assert.Error(t, typo.Validate())

	// Nesting would make "which source governs this node" depend on
	// resolution order rather than on the config.
	nested := brew()
	nested.Platforms = map[string]*VersionSource{"linux": brew()}
	deep := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b",
		Platforms: map[string]*VersionSource{"darwin": nested}}
	assert.Error(t, deep.Validate())

	empty := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b",
		Platforms: map[string]*VersionSource{"darwin": nil}}
	assert.Error(t, empty.Validate())

	// An override is validated like any other source.
	broken := &VersionSource{Type: VersionSourceGitHubRelease, Repo: "a/b",
		Platforms: map[string]*VersionSource{"darwin": {Type: VersionSourceHomebrew}}}
	assert.Error(t, broken.Validate())
}
