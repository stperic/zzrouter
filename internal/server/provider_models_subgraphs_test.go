package server

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderWorkflowTurboTemplates(t *testing.T) {
	for _, mode := range []string{"t2i", "image_edit"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/comfyui/image_qwen_image_2_1_turbo_" + mode + ".json")
			require.NoError(t, err)
			for _, envelope := range []bool{false, true} {
				body := raw
				if envelope {
					body, err = json.Marshal(map[string]any{"extra_data": map[string]any{"extra_pnginfo": map[string]any{"workflow": json.RawMessage(raw)}}})
					require.NoError(t, err)
				}
				items, err := workflowRequirements(body)
				require.NoError(t, err)
				require.Len(t, items, 4)
				require.Equal(t, "qwen_image_2.1_turbo_int8_convrot.safetensors", items[0].Name)
				require.Equal(t, "qwen3vl_8b_int8_convrot.safetensors", items[1].Name)
				require.Equal(t, "vae", items[2].Category)
			}
		})
	}
}

func TestProviderWorkflowSubgraphTraversal(t *testing.T) {
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	model := func(name string) workflowNode {
		n := workflowNode{Type: "CustomUnknownNode"}
		n.Properties.Models = []workflowModel{{Name: name, URL: "org/repo#" + name, Directory: "vae"}}
		return n
	}
	a, b, unused := model("a"), model("b"), model("unused")
	root := modelWorkflow{Nodes: []*workflowNode{{Type: first}, {Type: first}, &a}}
	inner := workflowSubgraph{ID: second, modelWorkflow: modelWorkflow{Nodes: []*workflowNode{&b, {Type: first}}}}
	outer := workflowSubgraph{ID: first, modelWorkflow: modelWorkflow{Nodes: []*workflowNode{{Type: second}, &a}}}
	outer.Definitions.Subgraphs = []workflowSubgraph{inner}
	root.Definitions.Subgraphs = []workflowSubgraph{outer, {ID: "33333333-3333-4333-8333-333333333333", modelWorkflow: modelWorkflow{Nodes: []*workflowNode{&unused}}}}
	raw, err := json.Marshal(root)
	require.NoError(t, err)
	items, err := workflowRequirements(raw)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "a", items[0].Name)
	require.Equal(t, "b", items[1].Name)
	extra := model("a")
	extra.Properties.Models[0].URL = "org/other#a"
	root.Nodes = append(root.Nodes, &extra)
	raw, err = json.Marshal(root)
	require.NoError(t, err)
	items, err = workflowRequirements(raw)
	require.NoError(t, err)
	require.Equal(t, "conflicting_source", items[0].Reason)
}

func TestProviderWorkflowMalformedSubgraphsBeforeAdmission(t *testing.T) {
	h := newProviderModelHarness(t)
	for _, raw := range []string{
		`{"nodes":[null]}`,
		`{"nodes":[{"type":"11111111-1111-4111-8111-111111111111"}]}`,
		`{"nodes":[],"definitions":{"subgraphs":[{"id":"","nodes":[]}]}}`,
		`{"nodes":[],"definitions":{"subgraphs":[{"id":"legacy","nodes":[]},{"id":"legacy","nodes":[{"type":"CustomNode"}]}]}}`,
		`{"nodes":[],"definitions":{"subgraphs":{}}}`,
	} {
		response := h.request("POST", "studio/models/resolve", resolveBody(t, json.RawMessage(raw), true), "")
		require.Equal(t, 400, response.Code, response.Body.String())
	}
	require.Zero(t, h.cluster.calls.Load())
}

func TestProviderWorkflowEquivalentLegacyDefinitions(t *testing.T) {
	raw := json.RawMessage(`{"nodes":[{"type":"legacy-graph"}],"definitions":{"subgraphs":[{"id":"legacy-graph","nodes":[{"properties":{"models":[{"name":"one","url":"org/repo#one","directory":"vae"}]}}]},{"id":"legacy-graph","nodes":[{"properties":{"models":[{"name":"one","url":"org/repo#one","directory":"vae"}]}}]}]}}`)
	items, err := workflowRequirements(raw)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "one", items[0].Name)
}
