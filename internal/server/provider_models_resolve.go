package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
)

const providerWorkflowMaxBytes = provapps.MaxWorkflowBytes

type providerModelResolveRequest struct {
	Node     string          `json:"node" binding:"required"`
	Workflow json.RawMessage `json:"workflow,omitempty"`
	Template json.RawMessage `json:"template,omitempty"`
	Download bool            `json:"download"`
}

type providerModelResolution struct {
	Provider     string                     `json:"provider"`
	Node         string                     `json:"node"`
	Requirements []providerModelRequirement `json:"requirements"`
	Template     *provapps.WorkflowTemplate `json:"template,omitempty"`
}

type providerModelRequirement struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Source   string `json:"source,omitempty"`
	JobID    string `json:"job_id,omitempty"`
}

type workflowModel struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Directory string `json:"directory"`
}

type workflowNode struct {
	Type       string `json:"type"`
	Properties struct {
		Models []workflowModel `json:"models"`
	} `json:"properties"`
}

type modelWorkflow struct {
	Nodes       []*workflowNode `json:"nodes"`
	Definitions struct {
		Subgraphs []workflowSubgraph `json:"subgraphs"`
	} `json:"definitions"`
}

type workflowSubgraph struct {
	ID string `json:"id"`
	modelWorkflow
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
		return nil, invalidWorkflow("invalid workflow model metadata")
	}
	if doc.Nodes == nil {
		doc.modelWorkflow = doc.ExtraData.ExtraPNGInfo.Workflow
	}
	models, err := workflowModels(&doc.modelWorkflow)
	if err != nil {
		return nil, err
	}
	var items []providerModelRequirement
	seen := map[[2]string]int{}
	for _, model := range models {
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
	if len(items) == 0 {
		return nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeWorkflowModelMetadataMissing),
			Message: "workflow carries no model metadata; model provisioning is unverified; supply a UI workflow with declared model sources",
			Extra:   map[string]any{"model_provisioning": "unverified"}}
	}
	return items, nil
}

func invalidWorkflow(detail string) error {
	return newProblemError(http.StatusBadRequest, "Bad Request", detail)
}

func workflowModels(root *modelWorkflow) ([]workflowModel, error) {
	definitions := map[string]*modelWorkflow{}
	graphs := []*modelWorkflow{root}
	for i := 0; i < len(graphs); i++ {
		for j := range graphs[i].Definitions.Subgraphs {
			subgraph := &graphs[i].Definitions.Subgraphs[j]
			if strings.TrimSpace(subgraph.ID) == "" {
				return nil, invalidWorkflow("invalid workflow subgraph id")
			}
			if previous, exists := definitions[subgraph.ID]; exists {
				if !reflect.DeepEqual(previous, &subgraph.modelWorkflow) {
					return nil, invalidWorkflow("conflicting workflow subgraph definitions")
				}
				continue
			}
			definitions[subgraph.ID] = &subgraph.modelWorkflow
			graphs = append(graphs, &subgraph.modelWorkflow)
		}
	}
	models := []workflowModel{}
	visited := map[string]bool{}
	graphs = []*modelWorkflow{root}
	for i := 0; i < len(graphs); i++ {
		for _, node := range graphs[i].Nodes {
			if node == nil {
				return nil, invalidWorkflow("invalid workflow node")
			}
			models = append(models, node.Properties.Models...)
			subgraph, exists := definitions[node.Type]
			if !exists {
				if workflowGraphID(node.Type) {
					return nil, invalidWorkflow("workflow subgraph reference has no definition")
				}
				continue
			}
			if !visited[node.Type] {
				visited[node.Type] = true
				graphs = append(graphs, subgraph)
			}
		}
	}
	return models, nil
}

func workflowGraphID(value string) bool {
	return len(value) == 36 && uuid.Validate(value) == nil
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
	if (len(req.Workflow) == 0) == (len(req.Template) == 0) {
		return nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeRequired), Message: "supply exactly one workflow object or template selector"}
	}
	var selected *provapps.WorkflowTemplate
	if len(req.Template) != 0 {
		var selector string
		if json.Unmarshal(req.Template, &selector) != nil || strings.TrimSpace(selector) == "" {
			return nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeInvalidValue), Message: "template must be a nonempty string"}
		}
		var err error
		selected, req.Workflow, err = s.templates.Resolve(ctx, selector)
		if err != nil {
			return nil, err
		}
		if selected.OpenSource != nil && !*selected.OpenSource {
			return nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeTemplateUnsupported), Message: "API-only templates do not install local model files"}
		}
	}
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
	result := &providerModelResolution{Provider: provider, Node: inventory.Node, Requirements: items, Template: selected}
	if selected != nil {
		selected.ModelProvisioning = "supported"
		for _, item := range items {
			if item.Status == "unresolvable" {
				selected.ModelProvisioning = "unsupported"
				break
			}
		}
	}
	return result, nil
}
