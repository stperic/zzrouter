package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsOpenAICompatPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/files":            true,
		"/v1/chat/completions": true,
		"/v1":                  true,
		"/v1/":                 true,
		"/api/tags":            false,
		"/zzrouter/v1/keys":    false,
		"/health":              false,
		"":                     false,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			if got := isOpenAICompatPath(path); got != want {
				t.Errorf("isOpenAICompatPath(%q) = %v, want %v", path, got, want)
			}
		})
	}
}

func TestIsHTMLResponse(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"explicit text/html", "text/html; charset=utf-8", "<html>...", true},
		{"explicit xhtml", "application/xhtml+xml", "<?xml ...", true},
		{"json content type", "application/json", `{"ok":true}`, false},
		{"sse content type", "text/event-stream", "data: {}\n\n", false},
		{"ndjson content type", "application/x-ndjson", `{"a":1}` + "\n", false},
		{"empty content type with html body", "", "<!DOCTYPE html><html>...", true},
		{"empty content type with json body", "", `{"foo":"bar"}`, false},
		{"empty content type empty body", "", "", false},
		{"plaintext body", "text/plain", "hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{},
				Body:   io.NopCloser(strings.NewReader(tt.body)),
			}
			if tt.contentType != "" {
				resp.Header.Set("Content-Type", tt.contentType)
			}
			got := isHTMLResponse(resp)
			if got != tt.want {
				t.Errorf("isHTMLResponse() = %v, want %v", got, tt.want)
			}
			// Body must remain readable downstream — sniff cannot consume it.
			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("body unreadable after sniff: %v", err)
			}
			combined := append([]byte{}, rest...)
			if !bytes.Equal(combined, []byte(tt.body)) {
				// When sniff peeks (empty content-type case) the body is
				// re-attached via MultiReader. The resulting bytes should
				// match the original.
				if string(rest) != tt.body {
					t.Errorf("body lost data after sniff: got %q want %q", rest, tt.body)
				}
			}
		})
	}
}
