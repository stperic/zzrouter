package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListInstancesResponse_Data(t *testing.T) {
	instances := []Instance{
		{ID: "inst1", Model: "llama3"},
		{ID: "inst2", Model: "mistral"},
	}
	resp := ListInstancesResponse{Data: instances, Total: 2}
	assert.Equal(t, instances, resp.Data)
	assert.Equal(t, 2, resp.Total)
}

func TestInstance_Fields(t *testing.T) {
	instance := Instance{
		ID:            "abc123def456",
		Node:          "localhost",
		App:           "ollama",
		LaunchMode:    "cli",
		Model:         "llama3:8b",
		SourceRepo:    "ollama",
		SizeBytes:     4_000_000_000,
		Processor:     "gpu",
		ContextLength: 8192,
		Port:          11434,
		Status:        "running",
		HealthURL:     "http://localhost:11434/api/tags",
		ProcessID:     12345,
		StartedAt:     "2024-01-15T10:00:00Z",
		KeepAlive:     "5m",
		LastActivity:  "2024-01-15T10:05:00Z",
		LaunchCommand: &LaunchCommand{
			Command:    "ollama",
			Args:       []string{"serve"},
			WorkingDir: "/home/user",
		},
	}

	assert.Equal(t, "abc123def456", instance.ID)
	assert.Equal(t, "localhost", instance.Node)
	assert.Equal(t, "ollama", instance.App)
	assert.Equal(t, "llama3:8b", instance.Model)
	assert.Equal(t, int64(4_000_000_000), instance.SizeBytes)
	assert.Equal(t, 8192, instance.ContextLength)
	assert.Equal(t, 11434, instance.Port)
	assert.Equal(t, "running", instance.Status)
	assert.Equal(t, 12345, instance.ProcessID)
	assert.Equal(t, "5m", instance.KeepAlive)
	assert.NotNil(t, instance.LaunchCommand)
	assert.Equal(t, "ollama", instance.LaunchCommand.Command)
	assert.Equal(t, []string{"serve"}, instance.LaunchCommand.Args)
}

func TestLaunchCommand_Fields(t *testing.T) {
	cmd := LaunchCommand{
		Command: "python",
		Args:    []string{"-m", "vllm.entrypoints.openai.api_server"},
		Environment: map[string]string{
			"CUDA_VISIBLE_DEVICES": "0,1",
			"HF_TOKEN":             "token123",
		},
		WorkingDir: "/opt/vllm",
	}

	assert.Equal(t, "python", cmd.Command)
	assert.Len(t, cmd.Args, 2)
	assert.Equal(t, "-m", cmd.Args[0])
	assert.Len(t, cmd.Environment, 2)
	assert.Equal(t, "0,1", cmd.Environment["CUDA_VISIBLE_DEVICES"])
	assert.Equal(t, "/opt/vllm", cmd.WorkingDir)
}

func TestRunPreview_Fields(t *testing.T) {
	preview := RunPreview{
		Model:       "llama3:8b",
		App:         "llama.cpp",
		Node:        "localhost",
		Port:        8080,
		Command:     "llama-server",
		Args:        []string{"--model", "/models/llama3.gguf"},
		FullCommand: "llama-server --model /models/llama3.gguf",
		WorkingDir:  "/opt/llama",
		Parameters: map[string]string{
			"ctx_size": "4096",
		},
		Environment: map[string]string{
			"CUDA_VISIBLE_DEVICES": "0",
		},
		ParameterSources: map[string]string{
			"ctx_size": "default",
		},
	}

	assert.Equal(t, "llama3:8b", preview.Model)
	assert.Equal(t, "llama.cpp", preview.App)
	assert.Equal(t, "localhost", preview.Node)
	assert.Equal(t, 8080, preview.Port)
	assert.Equal(t, "llama-server", preview.Command)
	assert.Contains(t, preview.FullCommand, "llama-server")
	assert.Equal(t, "4096", preview.Parameters["ctx_size"])
	assert.Equal(t, "default", preview.ParameterSources["ctx_size"])
}

func TestLoadModelRequest_Fields(t *testing.T) {
	req := LoadModelRequest{
		Node:      "gpu-server",
		App:       "vllm",
		ModelName: "meta-llama/Llama-3-8B",
		Force:     true,
		Parameters: map[string]string{
			"tensor-parallel-size": "2",
		},
		Environment: map[string]string{
			"CUDA_VISIBLE_DEVICES": "0,1",
		},
	}

	assert.Equal(t, "gpu-server", req.Node)
	assert.Equal(t, "vllm", req.App)
	assert.Equal(t, "meta-llama/Llama-3-8B", req.ModelName)
	assert.True(t, req.Force)
	assert.Equal(t, "2", req.Parameters["tensor-parallel-size"])
	assert.Equal(t, "0,1", req.Environment["CUDA_VISIBLE_DEVICES"])
}

func TestDeployRequest_Fields(t *testing.T) {
	req := DeployRequest{
		Model:    "TheBloke/Llama-2-7B-Chat-GGUF",
		Nodes:    []string{"localhost", "worker-1"},
		Registry: "huggingface",
		File:     "llama-2-7b-chat.Q4_K_M.gguf",
		Force:    true,
	}

	assert.Equal(t, "TheBloke/Llama-2-7B-Chat-GGUF", req.Model)
	assert.Equal(t, []string{"localhost", "worker-1"}, req.Nodes)
	assert.Equal(t, "huggingface", req.Registry)
	assert.Equal(t, "llama-2-7b-chat.Q4_K_M.gguf", req.File)
	assert.True(t, req.Force)
}

func TestUnloadModelRequest_Fields(t *testing.T) {
	req := UnloadModelRequest{
		ModelName: "llama3:8b",
	}

	assert.Equal(t, "llama3:8b", req.ModelName)
}

func TestErrorResponse_Fields(t *testing.T) {
	tests := []struct {
		name     string
		response ErrorResponse
	}{
		{
			name: "OpenAI-style plain error string",
			response: ErrorResponse{
				Error: "model not found",
			},
		},
		{
			name: "RFC 9457 detail field",
			response: ErrorResponse{
				Detail: "The requested model 'llama3' was not found in the registry",
			},
		},
		{
			name: "RFC 9457 title field",
			response: ErrorResponse{
				Title: "Not Found",
			},
		},
		{
			name: "all fields populated",
			response: ErrorResponse{
				Error:  map[string]any{"type": "not_found", "message": "Model not found"},
				Title:  "Not Found",
				Detail: "The model llama3 is not available",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = tt.response.Error
			_ = tt.response.Title
			_ = tt.response.Detail
		})
	}
}

func TestFilteredModelsResponse_Fields(t *testing.T) {
	resp := FilteredModelsResponse{
		NodeName: "localhost",
		NodeAddr: "127.0.0.1:9090",
		Apps: []FilteredProviderResult{
			{
				Name:     "ollama",
				Type:     "ollama",
				NodeName: "localhost",
				Models: []FilteredModelResult{
					{
						Name:     "llama3:8b",
						FullID:   "ollama/llama3:8b",
						Status:   "available",
						MemoryMB: 8192,
						Size:     4_000_000_000,
						Modified: "2024-01-15T10:00:00Z",
						Digest:   "sha256:abc123",
					},
				},
			},
		},
	}

	assert.Equal(t, "localhost", resp.NodeName)
	assert.Equal(t, "127.0.0.1:9090", resp.NodeAddr)
	assert.Len(t, resp.Apps, 1)
	assert.Equal(t, "ollama", resp.Apps[0].Name)
	assert.Len(t, resp.Apps[0].Models, 1)
	assert.Equal(t, "llama3:8b", resp.Apps[0].Models[0].Name)
}

func TestFilteredModelResult_Fields(t *testing.T) {
	model := FilteredModelResult{
		Name:          "llama3:8b",
		FullID:        "meta-llama/Meta-Llama-3-8B-Instruct",
		Status:        "running",
		MemoryMB:      8192,
		Size:          4_000_000_000,
		Modified:      "2024-01-15T10:00:00Z",
		Digest:        "sha256:abc123def456",
		ExpiresAt:     "2024-01-15T11:00:00Z",
		ContextLength: 8192,
		SizeVRAM:      8_000_000_000,
		Format:        "gguf",
		Family:        "llama",
		ParameterSize: "8B",
		QuantLevel:    "Q4_K_M",
	}

	assert.Equal(t, "llama3:8b", model.Name)
	assert.Equal(t, "meta-llama/Meta-Llama-3-8B-Instruct", model.FullID)
	assert.Equal(t, "running", model.Status)
	assert.Equal(t, int64(8192), model.MemoryMB)
	assert.Equal(t, int64(4_000_000_000), model.Size)
	assert.Equal(t, 8192, model.ContextLength)
	assert.Equal(t, "gguf", model.Format)
	assert.Equal(t, "llama", model.Family)
	assert.Equal(t, "8B", model.ParameterSize)
	assert.Equal(t, "Q4_K_M", model.QuantLevel)
}
