package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeMemoryPreservesAbsentAndMeasuredZeroUsage(t *testing.T) {
	var node NodeWithProviders
	require.NoError(t, json.Unmarshal([]byte(`{"memory":{"total_gb":64,"available_gb":32,"sample":{"total_bytes":68719476736}}}`), &node))
	require.Nil(t, node.MemoryInfo.UsedPercent)
	require.Nil(t, node.MemoryInfo.Sample.UsedBytes)
	require.NoError(t, json.Unmarshal([]byte(`{"memory":{"used_percent":0,"sample":{"used_bytes":0}}}`), &node))
	require.Equal(t, new(0.0), node.MemoryInfo.UsedPercent)
	require.Equal(t, new(int64(0)), node.MemoryInfo.Sample.UsedBytes)
}
