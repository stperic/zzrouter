package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
)

const providerWorkflowMaxBytes = 4 << 20

type providerModelResolveRequest struct {
	Node     string          `json:"node" binding:"required"`
	Workflow json.RawMessage `json:"workflow" binding:"required"`
	Download bool            `json:"download"`
}

type providerModelResolution struct {
	Provider     string                     `json:"provider"`
	Node         string                     `json:"node"`
	Requirements []providerModelRequirement `json:"requirements"`
}

type providerModelRequirement struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Source   string `json:"source,omitempty"`
	JobID    string `json:"job_id,omitempty"`
}

type modelWorkflow struct {
	Nodes []struct {
		Properties struct {
			Models []struct {
				Name      string `json:"name"`
				URL       string `json:"url"`
				Directory string `json:"directory"`
			} `json:"models"`
		} `json:"properties"`
	} `json:"nodes"`
}

func workflowRequirements(raw json.RawMessage) ([]providerModelRequirement, error) {
	var doc struct {
		modelWorkflow
		ExtraData struct {
			ExtraPNGInfo struct {
				Workflow modelWorkflow `json:"workflow"`
			} `json:"extra_pnginfo"`
		} `json:"extra_data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", "invalid workflow model metadata")
	}
	if doc.Nodes == nil {
		doc.modelWorkflow = doc.ExtraData.ExtraPNGInfo.Workflow
	}
	var items []providerModelRequirement
	seen := map[[2]string]int{}
	for _, node := range doc.Nodes {
		for _, model := range node.Properties.Models {
			if err := provapps.ValidateModelFileName(model.Name); err != nil {
				return nil, err
			}
			key := [2]string{model.Directory, model.Name}
			if i, exists := seen[key]; exists {
				item := &items[i]
				if workflowSourcesConflict(item.Source, model.URL) {
					item.Reason = "conflicting_source"
				}
				if item.Source == "" {
					item.Source = model.URL
				}
				continue
			}
			seen[key] = len(items)
			items = append(items, providerModelRequirement{Category: model.Directory, Name: model.Name, Source: model.URL})
		}
	}
	if len(items) == 0 {
		return nil, newProblemError(http.StatusBadRequest, "Bad Request", "workflow carries no model metadata; supply UI nodes[].properties.models or extra_data.extra_pnginfo.workflow")
	}
	return items, nil
}

func workflowSourcesConflict(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	old, oldErr := huggingface.ParseSource(a)
	next, nextErr := huggingface.ParseSource(b)
	return oldErr != nil || nextErr != nil || old.Repo != next.Repo || old.Revision != next.Revision || old.Weights != next.Weights
}

// ResolveProviderModels inspects one node and admits downloads through the existing service.
func (s *ProvidersService) ResolveProviderModels(ctx context.Context, provider string, req providerModelResolveRequest) (*providerModelResolution, error) {
	items, err := workflowRequirements(req.Workflow)
	if err != nil {
		return nil, err
	}
	inventory, err := s.ListProviderModels(ctx, provider, req.Node, "")
	if err != nil {
		return nil, err
	}
	present := map[string]map[string]bool{}
	for _, category := range inventory.Categories {
		present[category.Name] = map[string]bool{}
		for _, file := range category.Files {
			present[category.Name][file.Name] = file.SizeBytes > 0
		}
	}
	for i := range items {
		item := &items[i]
		files, validCategory := present[item.Category]
		switch {
		case !validCategory:
			item.Reason = string(httperr.CodeInvalidModelCategory)
		case item.Reason == "conflicting_source":
		case files[item.Name]:
			item.Status = "present"
			continue
		case item.Source == "":
			item.Reason = "missing_source"
		default:
			if _, err := huggingface.ParseSource(item.Source); err != nil {
				item.Reason = "source_unsupported"
			}
		}
		if item.Reason != "" {
			item.Status = "unresolvable"
			continue
		}
		item.Status = "missing"
		if req.Download {
			download, err := s.DownloadProviderModel(ctx, provider, providerModelDownloadRequest{Node: req.Node, Category: item.Category, Name: item.Name, Source: item.Source})
			if err != nil {
				var failure *RoutedError
				if !errors.As(err, &failure) || failure.ProblemDetails == nil || failure.ProblemDetails.Code == "" {
					failure = newRoutedTransportError(err, req.Node)
				}
				item.Status, item.Reason = "failed", failure.ProblemDetails.Code
				continue
			}
			item.Status, item.JobID = "downloading", download.JobID
		}
	}
	return &providerModelResolution{Provider: provider, Node: inventory.Node, Requirements: items}, nil
}
