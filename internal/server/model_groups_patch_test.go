package server

import (
	"encoding/json"
	"testing"
	"time"

	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleGroup() modelgroup.ModelGroup {
	return modelgroup.ModelGroup{
		Description: "primary chat route",
		Strategy:    modelgroup.StrategyPriority,
		HealthCheck: &modelgroup.HealthCheckConfig{
			Path:     "/health",
			Interval: modelgroup.Duration{Duration: 30 * time.Second},
			Timeout:  modelgroup.Duration{Duration: 5 * time.Second},
		},
		Replicas: []modelgroup.Replica{
			{Name: "r1", Model: "m", App: "ollama", Priority: 1},
		},
		Params: map[string]any{"region": "us", "tier": "free"},
	}
}

func TestApplyPatch_Description(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"description":"renamed"}`))
		require.Nil(t, err)
		assert.Equal(t, "renamed", out.Description)
	})
	t.Run("null clears", func(t *testing.T) {
		out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"description":null}`))
		require.Nil(t, err)
		assert.Equal(t, "", out.Description)
	})
	t.Run("absent preserves", func(t *testing.T) {
		out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{}`))
		require.Nil(t, err)
		assert.Equal(t, "primary chat route", out.Description)
	})
	t.Run("wrong type", func(t *testing.T) {
		_, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"description":42}`))
		require.NotNil(t, err)
		assert.Equal(t, "description", err.Key)
	})
}

func TestApplyPatch_HealthCheck_NestedMerge(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"health_check":{"interval":"1m"}}`))
	require.Nil(t, err)
	require.NotNil(t, out.HealthCheck)
	assert.Equal(t, "/health", out.HealthCheck.Path, "path preserved across partial merge")
	assert.Equal(t, time.Minute, out.HealthCheck.Interval.Duration)
	assert.Equal(t, 5*time.Second, out.HealthCheck.Timeout.Duration, "timeout preserved")
}

func TestApplyPatch_HealthCheck_NullClears(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"health_check":null}`))
	require.Nil(t, err)
	assert.Nil(t, out.HealthCheck)
}

func TestApplyPatch_HealthCheck_LeafNull(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"health_check":{"timeout":null}}`))
	require.Nil(t, err)
	require.NotNil(t, out.HealthCheck)
	assert.Equal(t, time.Duration(0), out.HealthCheck.Timeout.Duration)
	assert.Equal(t, 30*time.Second, out.HealthCheck.Interval.Duration)
}

func TestApplyPatch_HealthCheck_InvalidDuration(t *testing.T) {
	_, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"health_check":{"interval":"not-a-duration"}}`))
	require.NotNil(t, err)
	assert.Equal(t, "health_check.interval", err.Key)
}

func TestApplyPatch_ReplicasWholeReplace(t *testing.T) {
	body := `{"replicas":[{"name":"r2","model":"m2","provider":"vllm","priority":1}]}`
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(body))
	require.Nil(t, err)
	require.Len(t, out.Replicas, 1)
	assert.Equal(t, "r2", out.Replicas[0].Name)
	assert.Equal(t, "vllm", out.Replicas[0].App)
}

func TestApplyPatch_Params_PerKeyMergeAndDelete(t *testing.T) {
	body := `{"params":{"tier":null,"region":"eu","new_key":"x"}}`
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(body))
	require.Nil(t, err)
	_, hasTier := out.Params["tier"]
	assert.False(t, hasTier, "tier should be deleted")
	assert.Equal(t, "eu", out.Params["region"])
	assert.Equal(t, "x", out.Params["new_key"])
}

func TestApplyPatch_Strategy(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"strategy":"least-load"}`))
	require.Nil(t, err)
	assert.Equal(t, modelgroup.StrategyLeastLoad, out.Strategy)
}

func TestApplyPatch_InvalidBody(t *testing.T) {
	_, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`["not","object"]`))
	require.NotNil(t, err)
}

func TestApplyPatch_StrategyNullRevertsToServerDefault(t *testing.T) {
	g := sampleGroup()
	g.Strategy = modelgroup.StrategyFastest
	out, err := applyModelGroupPatch(g, json.RawMessage(`{"strategy":null}`))
	require.Nil(t, err)
	assert.Equal(t, modelgroup.StrategyType(""), out.Strategy,
		"null clears so validateGroup can re-apply the priority default on Set")
}

func TestApplyPatch_StrategyUnknownRejected(t *testing.T) {
	_, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"strategy":"bogus"}`))
	require.NotNil(t, err)
	assert.Equal(t, "strategy", err.Key)
}

func TestApplyPatch_ParamsTopLevelNullClears(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"params":null}`))
	require.Nil(t, err)
	assert.Nil(t, out.Params, "top-level params:null must clear the entire map per RFC 7396")
}

func TestApplyPatch_ReplicasNullClears(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"replicas":null}`))
	require.Nil(t, err)
	assert.Nil(t, out.Replicas)
}

func TestApplyPatch_ReplicasEmptyArrayReplaces(t *testing.T) {
	out, err := applyModelGroupPatch(sampleGroup(), json.RawMessage(`{"replicas":[]}`))
	require.Nil(t, err)
	assert.Empty(t, out.Replicas,
		"validateGroup tolerates empty replicas at the group-level mutator path; the replica-level DELETE handler carries the last-in-group guard")
}

func TestApplyReplicaPatch_PartialFieldsMerge(t *testing.T) {
	current := modelgroup.Replica{
		Name: "r1", Model: "m", App: "ollama", Priority: 1,
		Tags:    []string{"gpu", "fast"},
		Timeout: modelgroup.Duration{Duration: 30 * time.Second},
	}
	body := `{"priority":5,"timeout":"1m"}`
	out, err := applyReplicaPatch(current, json.RawMessage(body))
	require.Nil(t, err)
	assert.Equal(t, 5, out.Priority)
	assert.Equal(t, time.Minute, out.Timeout.Duration)
	assert.Equal(t, "m", out.Model, "absent fields preserved")
	assert.Equal(t, []string{"gpu", "fast"}, out.Tags)
}

func TestApplyReplicaPatch_TagsNullClears(t *testing.T) {
	current := modelgroup.Replica{Name: "r1", Tags: []string{"x"}}
	out, err := applyReplicaPatch(current, json.RawMessage(`{"tags":null}`))
	require.Nil(t, err)
	assert.Nil(t, out.Tags)
}

func TestApplyReplicaPatch_TagsWholeReplace(t *testing.T) {
	current := modelgroup.Replica{Name: "r1", Tags: []string{"old"}}
	out, err := applyReplicaPatch(current, json.RawMessage(`{"tags":["new"]}`))
	require.Nil(t, err)
	assert.Equal(t, []string{"new"}, out.Tags)
}

func TestApplyReplicaPatch_NullClearsScalar(t *testing.T) {
	current := modelgroup.Replica{Name: "r1", Node: "worker", Priority: 7, OnDemand: true}
	out, err := applyReplicaPatch(current, json.RawMessage(`{"node":null,"priority":null,"on_demand":null}`))
	require.Nil(t, err)
	assert.Equal(t, "", out.Node)
	assert.Equal(t, 0, out.Priority)
	assert.False(t, out.OnDemand)
}

func TestApplyReplicaPatch_WrongType(t *testing.T) {
	_, err := applyReplicaPatch(modelgroup.Replica{Name: "r1"}, json.RawMessage(`{"priority":"high"}`))
	require.NotNil(t, err)
	assert.Equal(t, "priority", err.Key)
}

func TestApplyReplicaPatch_InvalidTimeout(t *testing.T) {
	_, err := applyReplicaPatch(modelgroup.Replica{Name: "r1"}, json.RawMessage(`{"timeout":"bogus"}`))
	require.NotNil(t, err)
	assert.Equal(t, "timeout", err.Key)
}

func TestApplyReplicaPatch_ParamsMergeAndDelete(t *testing.T) {
	current := modelgroup.Replica{Name: "r1", Params: map[string]any{"a": 1.0, "b": "keep"}}
	out, err := applyReplicaPatch(current, json.RawMessage(`{"params":{"a":null,"c":"new"}}`))
	require.Nil(t, err)
	_, hasA := out.Params["a"]
	assert.False(t, hasA)
	assert.Equal(t, "keep", out.Params["b"])
	assert.Equal(t, "new", out.Params["c"])
}

func TestApplyPatch_LeavesCurrentUnmutatedOnError(t *testing.T) {
	g := sampleGroup()
	beforeReplicas := g.Replicas[0].Name
	_, err := applyModelGroupPatch(g, json.RawMessage(`{"strategy":"bogus"}`))
	require.NotNil(t, err)
	assert.Equal(t, beforeReplicas, g.Replicas[0].Name, "input must not be aliased into output on error")
	assert.Equal(t, modelgroup.StrategyPriority, g.Strategy)
}
