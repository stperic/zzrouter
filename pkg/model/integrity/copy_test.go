package integrity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDir(t *testing.T) {
	// Create temp directories
	tmpDir, err := os.MkdirTemp("", "modelcache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")

	// Create source directory with files
	if err := os.MkdirAll(filepath.Join(srcDir, "subdir"), 0755); err != nil {
		t.Fatalf("Failed to create source dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("content1"), 0644); err != nil {
		t.Fatalf("Failed to create file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "subdir", "file2.txt"), []byte("content2"), 0644); err != nil {
		t.Fatalf("Failed to create file2: %v", err)
	}

	// Copy directory
	if err := CopyDir(srcDir, dstDir); err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	// Verify files were copied
	content1, err := os.ReadFile(filepath.Join(dstDir, "file1.txt"))
	if err != nil {
		t.Fatalf("Failed to read copied file1: %v", err)
	}
	if string(content1) != "content1" {
		t.Errorf("file1 content mismatch: got %q, want %q", string(content1), "content1")
	}

	content2, err := os.ReadFile(filepath.Join(dstDir, "subdir", "file2.txt"))
	if err != nil {
		t.Fatalf("Failed to read copied file2: %v", err)
	}
	if string(content2) != "content2" {
		t.Errorf("file2 content mismatch: got %q, want %q", string(content2), "content2")
	}
}

func TestCopyDir_NonExistentSource(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "modelcache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	err = CopyDir(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "dst"))
	if err == nil {
		t.Error("CopyDir should fail for nonexistent source")
	}
}

func TestFileExists(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "modelcache-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "testfile")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	if !FileExists(testFile) {
		t.Error("FileExists should return true for existing file")
	}

	if FileExists(filepath.Join(tmpDir, "nonexistent")) {
		t.Error("FileExists should return false for nonexistent file")
	}
}
