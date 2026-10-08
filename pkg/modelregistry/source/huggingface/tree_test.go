package huggingface

import "testing"

// TestParseRepoFilesJSON_ExtractsLFSSHA pins the contract that LFS oids
// from HF's card API surface as TreeFileEntry.SHA256 — the download
// path uses this to reject CDN-served bytes that don't match upstream.
// Plain (non-LFS) entries leave SHA256 empty.
func TestParseRepoFilesJSON_ExtractsLFSSHA(t *testing.T) {
	body := []byte(`{"siblings":[
		{"rfilename":"config.json","size":512},
		{"rfilename":"model.gguf","size":1234567890,
		 "lfs":{"oid":"abc123def4567890","size":1234567890,"pointerSize":134}}
	]}`)
	files, err := parseRepoFilesJSON(body, "test/repo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	for _, f := range files {
		switch f.Name {
		case "config.json":
			if f.SHA256 != "" {
				t.Errorf("non-LFS file should have empty SHA256, got %q", f.SHA256)
			}
		case "model.gguf":
			if f.SHA256 != "abc123def4567890" {
				t.Errorf("LFS oid not extracted: got %q", f.SHA256)
			}
		}
	}
}
