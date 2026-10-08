package search

import (
	"context"
	"testing"
)

func TestIsCloudSearchProvider(t *testing.T) {
	// Non-cloud providers should not be registered
	if IsCloudSearchProvider(ProviderHuggingFace) {
		t.Error("HuggingFace should not be a cloud search provider")
	}
	if IsCloudSearchProvider(ProviderOllama) {
		t.Error("Ollama should not be a cloud search provider")
	}
	if IsCloudSearchProvider(Provider("nonexistent")) {
		t.Error("nonexistent should not be a cloud search provider")
	}
}

func TestSearchCloudProvider_Unknown(t *testing.T) {
	_, err := SearchCloudProvider(context.Background(), Provider("nonexistent"), "", 10)
	if err == nil {
		t.Error("SearchCloudProvider with unknown provider should return error")
	}
}

func TestParseProvider_ConfigDrivenProviders(t *testing.T) {
	// Config-driven providers are resolved via IsCloudSearchProvider fallthrough
	RegisterCloudProvider(Provider("test-provider"), CloudProviderMeta{
		DisplayName: "Test",
	}, func(ctx context.Context, query string, limit int) ([]any, error) {
		return nil, nil
	})
	defer func() {
		cloudRegistryMu.Lock()
		delete(cloudRegistry, Provider("test-provider"))
		cloudRegistryMu.Unlock()
	}()

	result, err := ParseProvider("test-provider")
	if err != nil {
		t.Fatalf("ParseProvider for config-driven provider failed: %v", err)
	}
	if result != Provider("test-provider") {
		t.Errorf("got %q, want %q", result, "test-provider")
	}

	if !IsCloudSearchProvider(Provider("test-provider")) {
		t.Error("dynamically registered provider should be a cloud search provider")
	}
}
