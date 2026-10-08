package mesh

import (
	"context"
	"encoding/json"
)

// AggregateArrayField is a DRY helper for broadcast + aggregation pattern
// This eliminates ~40 lines of repeated code per endpoint
//
// Usage:
//
//	items, err := cluster.AggregateArrayField(ctx, clusterClient, "/admin/providers", "providers", query)
func AggregateArrayField(
	ctx context.Context,
	clusterClient ClusterClient,
	endpoint string,
	arrayField string,
	query QueryParams,
) ([]map[string]any, error) {
	// Workers will automatically return their own local data
	broadcastResp, err := clusterClient.Broadcast(ctx, endpoint, &query)
	if err != nil {
		return nil, err
	}

	// Aggregate results from all hosts
	allItems := make([]map[string]any, 0)

	for _, hostResp := range broadcastResp.Responses {
		if hostResp.Error != nil || hostResp.Response == nil {
			continue
		}

		// Try parsing as object with array field first
		var hostData map[string]any
		if err := json.Unmarshal(hostResp.Response.Body, &hostData); err == nil {
			if items, ok := hostData[arrayField].([]any); ok {
				for _, item := range items {
					if itemMap, ok := item.(map[string]any); ok {
						allItems = append(allItems, itemMap)
					}
				}
			}
			continue
		}

		// Fallback: try parsing as direct array
		var itemsArray []map[string]any
		if err := json.Unmarshal(hostResp.Response.Body, &itemsArray); err == nil {
			allItems = append(allItems, itemsArray...)
		}
	}

	return allItems, nil
}
