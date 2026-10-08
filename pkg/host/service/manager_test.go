package service

import (
	"runtime"
	"testing"
)

func TestSortedKeys(t *testing.T) {
	m := map[string]string{"c": "3", "a": "1", "b": "2"}
	keys := SortedKeys(m)
	if len(keys) != 3 {
		t.Fatalf("Expected 3 keys, got %d", len(keys))
	}
	if keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Errorf("Keys not sorted: %v", keys)
	}
}

func TestSortedKeys_Empty(t *testing.T) {
	keys := SortedKeys(map[string]string{})
	if len(keys) != 0 {
		t.Errorf("Expected 0 keys, got %d", len(keys))
	}
}

func TestFormatEnvOverride(t *testing.T) {
	env := map[string]string{
		"OLLAMA_NUM_PARALLEL":    "4",
		"OLLAMA_FLASH_ATTENTION": "1",
	}
	result := FormatEnvOverride(env)

	if result == "" {
		t.Fatal("Expected non-empty result")
	}
	// Should start with [Service]
	if result[:10] != "[Service]\n" {
		t.Errorf("Expected [Service] header, got: %s", result[:20])
	}
	// Keys should be sorted (FLASH before NUM)
	flashIdx := len(result) // default
	numIdx := len(result)
	for i, line := range []byte(result) {
		if line == 'F' && i > 0 {
			flashIdx = i
		}
		if line == 'N' && i > 0 && numIdx == len(result) {
			numIdx = i
		}
	}
	if flashIdx > numIdx {
		t.Error("Keys should be sorted: OLLAMA_FLASH_ATTENTION before OLLAMA_NUM_PARALLEL")
	}
}

func TestGet_ValidManagers(t *testing.T) {
	for _, name := range []string{"systemd", "launchd", "nssm"} {
		mgr, err := Get(name)
		if err != nil {
			t.Errorf("Get(%q) returned error: %v", name, err)
		}
		if mgr.Name() != name {
			t.Errorf("Get(%q).Name() = %q", name, mgr.Name())
		}
	}
}

func TestGet_Invalid(t *testing.T) {
	_, err := Get("invalid")
	if err == nil {
		t.Error("Get('invalid') should return error")
	}
}

func TestPlatformManagers(t *testing.T) {
	managers := platformManagers()
	switch runtime.GOOS {
	case "linux":
		if len(managers) != 1 || managers[0].Name() != "systemd" {
			t.Errorf("Linux should have systemd manager, got: %v", managers)
		}
	case "darwin":
		if len(managers) != 1 || managers[0].Name() != "launchd" {
			t.Errorf("macOS should have launchd manager, got: %v", managers)
		}
	case "windows":
		if len(managers) != 1 || managers[0].Name() != "nssm" {
			t.Errorf("Windows should have nssm manager, got: %v", managers)
		}
	}
}

func TestDetect_NonexistentService(t *testing.T) {
	mgr := Detect("nonexistent-service-that-does-not-exist-12345")
	if mgr != nil {
		t.Errorf("Detect should return nil for nonexistent service, got: %s", mgr.Name())
	}
}
