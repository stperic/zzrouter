package config

import (
	"testing"
)

func TestNewConfigManager(t *testing.T) {
	cm := NewConfigManager("testapp")
	if cm == nil {
		t.Fatal("NewConfigManager returned nil")
	}
	if cm.appName != "testapp" {
		t.Errorf("Expected appName to be 'testapp', got '%s'", cm.appName)
	}
}

func TestConfigManager_GetClientConfigDir(t *testing.T) {
	cm := NewConfigManager("testapp")
	dir := cm.GetClientConfigDir()
	if dir == "" {
		t.Error("GetClientConfigDir returned empty string")
	}
	// Directory path should contain the app name
	if len(dir) < len("testapp") {
		t.Error("GetClientConfigDir returned suspiciously short path")
	}
}

func TestConfigManager_GetNodeConfigDir(t *testing.T) {
	cm := NewConfigManager("testapp")
	dir := cm.GetNodeConfigDir()
	if dir == "" {
		t.Error("GetNodeConfigDir returned empty string")
	}
	// Directory path should contain the app name
	if len(dir) < len("testapp") {
		t.Error("GetNodeConfigDir returned suspiciously short path")
	}
}

func TestConfigManager_GetClientConfigPath(t *testing.T) {
	cm := NewConfigManager("testapp")
	path := cm.GetClientConfigPath()
	if path == "" {
		t.Error("GetClientConfigPath returned empty string")
	}
	// Path should end with cli.yaml
	expectedSuffix := "cli.yaml"
	if len(path) < len(expectedSuffix) || path[len(path)-len(expectedSuffix):] != expectedSuffix {
		t.Errorf("Expected path to end with '%s', got '%s'", expectedSuffix, path)
	}
}

func TestConfigManager_GetNodeConfigPath(t *testing.T) {
	cm := NewConfigManager("testapp")
	path := cm.GetNodeConfigPath()
	if path == "" {
		t.Error("GetNodeConfigPath returned empty string")
	}
	// Path should end with node.yaml
	expectedSuffix := "node.yaml"
	if len(path) < len(expectedSuffix) || path[len(path)-len(expectedSuffix):] != expectedSuffix {
		t.Errorf("Expected path to end with '%s', got '%s'", expectedSuffix, path)
	}
}
