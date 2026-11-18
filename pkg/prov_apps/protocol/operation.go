package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ReadJSON executes one bounded diagnostic against an already-ready target.
// Redirects are never followed: a binding cannot transfer credentials to another origin.
func (t Target) ReadJSON(ctx context.Context, client *http.Client, method, path string, input any) (any, error) {
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.maxResponseBytes = 1 << 20
	response, err := t.call(ctx, &bounded, method, path, input)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("diagnostic upstream returned HTTP %d", response.StatusCode)
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("diagnostic upstream returned invalid JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("diagnostic upstream returned trailing JSON")
	}
	return result, nil
}
