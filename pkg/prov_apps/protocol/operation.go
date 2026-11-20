package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// ReadStatusError reports an unsuccessful diagnostic response.
type ReadStatusError int

func (e ReadStatusError) Error() string {
	return fmt.Sprintf("diagnostic upstream returned HTTP %d", e)
}

// ErrMalformedJSON identifies a diagnostic response that cannot be decoded.
var ErrMalformedJSON = errors.New("diagnostic upstream returned invalid JSON")

// ReadJSON decodes one bounded diagnostic response with the accepted success statuses.
func (t Target) ReadJSON(ctx context.Context, client *http.Client, method, path string, input any, accepted ...int) (any, error) {
	body, err := t.ReadText(ctx, client, method, path, input, accepted...)
	if err != nil {
		return nil, err
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedJSON, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON", ErrMalformedJSON)
	}
	return result, nil
}

// ReadText reads a bounded diagnostic without following redirects and accepts any 2xx by default.
func (t Target) ReadText(ctx context.Context, client *http.Client, method, path string, input any, accepted ...int) ([]byte, error) {
	bounded := http.Client{Transport: client.Transport, Timeout: client.Timeout}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.maxResponseBytes = 1 << 20
	response, err := t.call(ctx, &bounded, method, path, input)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || (len(accepted) > 0 && !slices.Contains(accepted, response.StatusCode)) {
		return nil, ReadStatusError(response.StatusCode)
	}
	return response.Body, nil
}
