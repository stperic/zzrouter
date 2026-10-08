package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// Every parameter write must go out as a Merge-Patch.
//
// The PUT and DELETE routes these methods used answer 410 Gone, so every
// parameter write in the TUI was failing: save, delete, the lot. Nothing
// caught it because no test asserted what the client puts on the wire.
// These do.

type capturedRequest struct {
	method      string
	path        string
	contentType string
	body        map[string]any
}

// paramsTestServer records the mutating request and answers reads with
// an empty-but-valid parameter payload.
func paramsTestServer(t *testing.T, got *[]capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			*got = append(*got, capturedRequest{
				method:      r.Method,
				path:        r.URL.Path,
				contentType: r.Header.Get("Content-Type"),
				body:        body,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/schema") {
			_, _ = w.Write([]byte(`{"parameters":{"threads":{"type":"int"},` +
				`"flash-attn":{"type":"bool"},"alias":{"type":"string"}},"environment":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"provider":"llamacpp","parameters":[],"environment":[]}`))
	}))
}

func TestParameterWrites_UseMergePatch(t *testing.T) {
	cases := []struct {
		name     string
		call     func(c *Client) error
		wantBody map[string]any
	}{
		{
			name: "provider tier",
			call: func(c *Client) error {
				_, err := c.UpdateProviderParameters("llamacpp",
					&UpdateParametersRequest{Parameters: map[string]string{"threads": "8"}}, false)
				return err
			},
			// 8, not "8": the validator type-checks against the schema
			// even though storage is strings.
			wantBody: map[string]any{"defaults": map[string]any{"parameters": map[string]any{"threads": float64(8)}}},
		},
		{
			name: "model tier",
			call: func(c *Client) error {
				_, err := c.UpdateModelParameters("llamacpp", "qwen",
					&UpdateParametersRequest{Parameters: map[string]string{"threads": "8"}}, false)
				return err
			},
			wantBody: map[string]any{"models": map[string]any{"qwen": map[string]any{"parameters": map[string]any{"threads": float64(8)}}}},
		},
		{
			name: "node tier",
			call: func(c *Client) error {
				_, err := c.UpdateNodeParameters("llamacpp", "worker-1",
					&UpdateParametersRequest{Environment: map[string]string{"CUDA": "1"}}, false)
				return err
			},
			wantBody: map[string]any{"nodes": map[string]any{"worker-1": map[string]any{"environment": map[string]any{"CUDA": "1"}}}},
		},
		{
			// null is Merge-Patch's delete. Sending an empty string here
			// would set the key to "" instead of removing it, which the
			// three-meaning rule treats as a different intent entirely.
			name: "delete uses a null, not an empty value",
			call: func(c *Client) error {
				return c.DeleteProviderParameter("llamacpp", "threads")
			},
			wantBody: map[string]any{"defaults": map[string]any{"parameters": map[string]any{"threads": nil}}},
		},
		{
			name: "delete an environment key at the model tier",
			call: func(c *Client) error {
				return c.DeleteModelEnvironment("llamacpp", "qwen", "CUDA")
			},
			wantBody: map[string]any{"models": map[string]any{"qwen": map[string]any{"environment": map[string]any{"CUDA": nil}}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []capturedRequest
			srv := paramsTestServer(t, &got)
			defer srv.Close()

			if err := tc.call(NewClient(pkgConfig.ClientNodeConfig{Name: "test", Address: srv.URL, APIKey: "test-key"})); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 mutating request, got %d", len(got))
			}
			req := got[0]

			// The retired verbs are the bug. Naming them keeps the test
			// honest about what it is defending against.
			if req.method == http.MethodPut || req.method == http.MethodDelete {
				t.Fatalf("method = %s, which is a retired route that answers 410", req.method)
			}
			if req.method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", req.method)
			}
			if req.contentType != mergePatchContentType {
				t.Errorf("Content-Type = %q, want %q (the server routes on it)",
					req.contentType, mergePatchContentType)
			}
			wantJSON, _ := json.Marshal(tc.wantBody)
			gotJSON, _ := json.Marshal(req.body)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("body\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

// A caller asking to validate must be told it cannot, not quietly obeyed
// with a save. Merge-Patch has no preview mode, so the old dry_run flag
// has no equivalent: converting it to a plain patch would turn the TUI's
// validate action into a silent write.
func TestValidateWithoutSaving_IsRefusedNotSaved(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"provider", func(c *Client) error {
			_, err := c.UpdateProviderParameters("llamacpp", &UpdateParametersRequest{}, true)
			return err
		}},
		{"model", func(c *Client) error {
			_, err := c.UpdateModelParameters("llamacpp", "qwen", &UpdateParametersRequest{}, true)
			return err
		}},
		{"node", func(c *Client) error {
			_, err := c.UpdateNodeParameters("llamacpp", "worker-1", &UpdateParametersRequest{}, true)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []capturedRequest
			srv := paramsTestServer(t, &got)
			defer srv.Close()

			err := tc.call(NewClient(pkgConfig.ClientNodeConfig{Name: "test", Address: srv.URL, APIKey: "test-key"}))
			if !errors.Is(err, ErrValidateWithoutSaving) {
				t.Fatalf("err = %v, want ErrValidateWithoutSaving", err)
			}
			if len(got) != 0 {
				t.Fatalf("a dry run sent %d mutating request(s); it must send none", len(got))
			}
		})
	}
}

// Values are typed against the schema before they go on the wire.
//
// Everything the editor collects is a string, but Merge-Patch rejects
// "6" for an int key with wrong_type, which is how every save of a
// numeric parameter would have failed even after the routes were fixed.
// Found by driving the real coordinator, not the mock.
func TestTypeValues_CoercesAgainstSchema(t *testing.T) {
	schema := map[string]ParameterSchema{
		"threads":    {Type: "int"},
		"flash-attn": {Type: "bool"},
		"alias":      {Type: "string"},
		"temp":       {Type: "float"},
	}
	got := typeValues(schema, map[string]string{
		"threads":    "6",
		"flash-attn": "true",
		"alias":      "my-model",
		"temp":       "0.7",
		"unknown":    "42",     // not in schema: pass through untouched
		"bad-int":    "twelve", // unparseable: let the server say so
	})
	want := map[string]any{
		"threads":    int64(6),
		"flash-attn": true,
		"alias":      "my-model",
		"temp":       0.7,
		"unknown":    "42",
		"bad-int":    "twelve",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
	// A string-typed key holding digits must stay a string, or a schema
	// expecting text would start receiving numbers.
	if _, isStr := typeValues(schema, map[string]string{"alias": "8192"})["alias"].(string); !isStr {
		t.Error("a string-typed key was coerced to a number")
	}
}
