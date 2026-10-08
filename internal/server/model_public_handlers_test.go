package server

import (
	"testing"
)

func TestListModelsRequest_Validate(t *testing.T) {
	tests := []struct {
		name      string
		request   *ListModelsRequest
		expectErr bool
	}{
		{
			name:      "empty request is valid",
			request:   &ListModelsRequest{},
			expectErr: false,
		},
		{
			name: "valid host filter",
			request: &ListModelsRequest{
				Node: "localhost",
			},
			expectErr: false,
		},
		{
			name: "valid app filter",
			request: &ListModelsRequest{
				App: "ollama",
			},
			expectErr: false,
		},
		{
			name: "valid model pattern",
			request: &ListModelsRequest{
				Model: "llama*",
			},
			expectErr: false,
		},
		{
			name: "multiple filters are valid",
			request: &ListModelsRequest{
				Node:  "localhost",
				App:   "vllm",
				Model: "qwen*",
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if (err != nil) != tt.expectErr {
				t.Errorf("Validate() error = %v, expectErr %v", err, tt.expectErr)
			}
		})
	}
}

func TestShowModelRequest_Validate(t *testing.T) {
	tests := []struct {
		name      string
		request   *ShowModelRequest
		expectErr bool
	}{
		{
			name: "valid request with model",
			request: &ShowModelRequest{
				ModelName: "llama3:8b",
			},
			expectErr: false,
		},
		{
			name: "valid request with model and app",
			request: &ShowModelRequest{
				ModelName: "llama3:8b",
				App:       "ollama",
			},
			expectErr: false,
		},
		{
			name:      "empty model is invalid",
			request:   &ShowModelRequest{},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if (err != nil) != tt.expectErr {
				t.Errorf("Validate() error = %v, expectErr %v", err, tt.expectErr)
			}
		})
	}
}

// Note: Full handler tests require model service to be initialized.
// These tests focus on request validation which can be tested in isolation.
// For full integration tests, see cluster_integration_test.go

// TestShowModelHandler_Validation tests that the show model handler properly validates input
// Note: This is a focused validation test - full handler tests require service initialization
func TestShowModelHandler_ValidationLogic(t *testing.T) {
	// Test the validation logic directly without going through HTTP layer
	// This avoids needing to set up the full service chain

	tests := []struct {
		name        string
		modelName   string
		expectError bool
	}{
		{
			name:        "empty model name fails validation",
			modelName:   "",
			expectError: true,
		},
		{
			name:        "valid model name passes validation",
			modelName:   "llama3:8b",
			expectError: false,
		},
		{
			name:        "model name with path passes validation",
			modelName:   "Qwen/Qwen2.5-VL-3B",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &ShowModelRequest{
				ModelName: tt.modelName,
			}
			err := req.Validate()
			if (err != nil) != tt.expectError {
				t.Errorf("Validate() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}
