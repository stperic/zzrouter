package install

import (
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func shippedRecipe(t *testing.T, provider, runtime string) config.InstallRecipe {
	t.Helper()
	data, err := templates.AppsFS.ReadFile(shippedOnDemandDir + "/" + provider + "/config.yaml")
	require.NoError(t, err)
	var shipped config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &shipped))
	require.NotNil(t, shipped.Install)
	recipe, ok := shipped.Install.Runtimes[runtime]
	require.True(t, ok, "%s/%s not shipped", provider, runtime)
	return recipe
}

func TestReleasePolicyAuthorizesEveryShippedRecipeOnItsPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch string
		grants       []string
	}{
		{"linux", "amd64", []string{"vllm/vllm"}},
		{"darwin", "arm64", []string{"mlx/mlx", "mlx/mlx-vlm"}},
		{"windows", "amd64", nil},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			policy, err := ReleasePolicy(tc.goos, tc.goarch)
			require.NoError(t, err)
			assert.Len(t, policy.Runtimes, len(tc.grants))
			for _, key := range tc.grants {
				grant, ok := policy.Runtimes[key]
				require.True(t, ok, "missing grant %s", key)
				provider, runtime, _ := strings.Cut(key, "/")
				assert.NoError(t, grant.Authorize(shippedRecipe(t, provider, runtime)))
				assert.False(t, grant.AllowPrivateIndexes)
			}
		})
	}
}

func TestReleasePolicyGrantsNothingBeyondTheShippedRecipe(t *testing.T) {
	policy, err := ReleasePolicy("linux", "amd64")
	require.NoError(t, err)
	grant := policy.Runtimes["vllm/vllm"]
	recipe := shippedRecipe(t, "vllm", "vllm")

	extra := "https://example.com/simple"
	widened := recipe
	widened.Indexes = &config.InstallIndexes{Primary: recipe.Indexes.Primary, Extra: &[]string{extra}}
	assert.Error(t, grant.Authorize(widened), "an index the release does not use must stay unapproved")

	imports := append(append([]string{}, *recipe.Verify.Imports...), "zzrouter_unshipped_module")
	widened = recipe
	widened.Verify = &config.InstallVerify{Imports: &imports, Checks: recipe.Verify.Checks}
	assert.Error(t, grant.Authorize(widened), "an import the release does not use must stay unapproved")
}

func TestMarshalReleasePolicyRoundTrips(t *testing.T) {
	body, err := MarshalReleasePolicy("darwin", "arm64")
	require.NoError(t, err)
	var parsed InstallPolicy
	require.NoError(t, yaml.Unmarshal(body, &parsed))
	want, err := ReleasePolicy("darwin", "arm64")
	require.NoError(t, err)
	assert.Equal(t, want, parsed)
}

func TestMarshalReleasePolicyRefusesAPlatformWithoutRecipes(t *testing.T) {
	_, err := MarshalReleasePolicy("windows", "amd64")
	assert.Error(t, err, "a policy without grants would be rejected by the node it is written for")
}

func TestReadInstallPolicyExplainsAMissingConfiguration(t *testing.T) {
	_, _, err := ReadInstallPolicy("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "providers.install_policy_file")
	assert.Contains(t, err.Error(), "install-policy template")
}
