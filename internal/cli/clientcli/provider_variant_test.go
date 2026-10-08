package clientcli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

type fakeVariantAPI struct {
	provider, model string
	update          pkgClient.ModelCellUpdate
	restart         bool
	deleted         string
}

func (f *fakeVariantAPI) UpdateModelCell(provider, model string, u pkgClient.ModelCellUpdate, restart bool) (*pkgClient.ProviderParametersWrite, error) {
	f.provider, f.model, f.update, f.restart = provider, model, u, restart
	return &pkgClient.ProviderParametersWrite{ProviderResolved: pkgClient.ProviderResolved{
		From: "Qwen3.8-27B-Q8_0",
		Parameters: map[string]pkgClient.ResolvedValue{
			"ctx-size": {Value: "131072", Tier: "model", Model: model},
		},
		Request: map[string]pkgClient.ResolvedValue{
			"chat_template_kwargs": {Value: map[string]any{"enable_thinking": false}, Tier: "model"},
		},
	}}, nil
}

func (f *fakeVariantAPI) DeleteModelCell(_, model string) error {
	f.deleted = model
	return nil
}

func (f *fakeVariantAPI) GetProviderResolved(_, _, model string) (*pkgClient.ProviderResolved, error) {
	if strings.Contains(model, "+") {
		return &pkgClient.ProviderResolved{From: "Qwen3.8-27B-Q8_0"}, nil
	}
	return &pkgClient.ProviderResolved{}, nil
}

func TestProvidersVariant(t *testing.T) {
	variant, _, err := NewProvidersCmd().Find([]string{"variant"})
	require.NoError(t, err)
	set, _, err := variant.Find([]string{"set"})
	require.NoError(t, err)
	assert.Equal(t, "set <provider> <name>", set.Use)
	for _, flag := range []string{"from", "param", "request", "unset-param", "unset-request", "restart"} {
		assert.NotNil(t, set.Flags().Lookup(flag), flag)
	}

	params := map[string]string{"ctx-size": "131072", "chat-template-file": "t.jinja"}
	request, err := parseRequest([]string{`chat_template_kwargs={"enable_thinking":false}`, "temperature=0.7", "stop=END"})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"temperature":          0.7,
		"stop":                 "END",
	}, request)
	_, err = parseRequest([]string{"no-equals"})
	assert.Error(t, err)

	api := &fakeVariantAPI{}
	cmd, out := newTestCmd("")
	u := pkgClient.ModelCellUpdate{From: "Qwen3.8-27B-Q8_0", Parameters: params, Request: request}
	require.NoError(t, runVariantSet(cmd, api, "llamacpp", "Qwen3.8-27B-Q8_0+agent", u, true))
	assert.Equal(t, "Qwen3.8-27B-Q8_0+agent", api.model)
	assert.True(t, api.restart)
	assert.Equal(t, u, api.update)
	assert.Contains(t, out.String(), "Variant of Qwen3.8-27B-Q8_0")
	assert.Regexp(t, `request chat_template_kwargs\s+\{"enable_thinking":false\}\s+model`, out.String())

	cmd, _ = newTestCmd("")
	assert.Error(t, runVariantSet(cmd, api, "llamacpp", "x", pkgClient.ModelCellUpdate{}, false), "nothing to set")
}

// The flags map onto the update the client sends, through real parsing.
func TestVariantUpdateFromFlags(t *testing.T) {
	parse := func(args ...string) (pkgClient.ModelCellUpdate, error) {
		cmd := newVariantSetCmd()
		require.NoError(t, cmd.ParseFlags(args))
		return variantUpdateFromFlags(cmd)
	}
	u, err := parse("--from", "base", "--param", "ctx-size=8192", "--request", "temperature=0.7",
		"--unset-param", "threads", "--unset-request", "top_p")
	require.NoError(t, err)
	assert.Equal(t, pkgClient.ModelCellUpdate{
		From:         "base",
		Parameters:   map[string]string{"ctx-size": "8192"},
		Unset:        []string{"threads"},
		Request:      map[string]any{"temperature": 0.7},
		UnsetRequest: []string{"top_p"},
	}, u)

	_, err = parse("--param", "ctx-size=8192", "--unset-param", "ctx-size")
	assert.ErrorContains(t, err, `both name "ctx-size"`)
	_, err = parse("--request", "temperature=0.7", "--unset-request", "temperature")
	assert.ErrorContains(t, err, `both name "temperature"`)
	_, err = parse("--param", "no-equals")
	assert.Error(t, err)
	_, err = parse("--param", "=x")
	assert.ErrorContains(t, err, "is not key=value", "an empty key is refused")
	u, err = parse("--param", "stop=a,b")
	require.NoError(t, err, "a comma is a value, not a second parameter: variant set has no --params")
	assert.Equal(t, "a,b", u.Parameters["stop"])
}

// rm deletes the whole models entry, so it removes only a variant.
func TestRunVariantRm(t *testing.T) {
	api := &fakeVariantAPI{}
	cmd, out := newTestCmd("")
	require.NoError(t, runVariantRm(cmd, api, "llamacpp", "Qwen3.8-27B-Q8_0+chat"))
	assert.Equal(t, "Qwen3.8-27B-Q8_0+chat", api.deleted)
	assert.Contains(t, out.String(), "Removed Qwen3.8-27B-Q8_0+chat from llamacpp")

	api = &fakeVariantAPI{}
	cmd, _ = newTestCmd("")
	assert.ErrorContains(t, runVariantRm(cmd, api, "llamacpp", "Qwen3.8-27B-Q8_0"), "is not a variant")
	assert.Empty(t, api.deleted, "a plain model's settings are left alone")
}
