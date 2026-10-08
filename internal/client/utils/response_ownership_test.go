package client

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type trackedResponseBody struct {
	io.Reader
	closes int
}

func (b *trackedResponseBody) Close() error { b.closes++; return nil }

func TestDoWithClosesResponseOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		payload   string
		target    any
		wantError bool
	}{
		{"json", 200, `{"ok":true}`, &map[string]any{}, false},
		{"bytes", 200, "asset", &[]byte{}, false},
		{"discard", 204, "", nil, false},
		{"status error", 400, `{"detail":"refused"}`, nil, true},
		{"decode error", 200, "{", &map[string]any{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedResponseBody{Reader: strings.NewReader(tc.payload)}
			send := func(string, string, any) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: body}, nil
			}
			err := (&Client{}).doWith(send, "GET", "/", nil, tc.target, "read")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, body.closes)
		})
	}
	t.Run("transport error", func(t *testing.T) {
		want := errors.New("transport failed")
		err := (&Client{}).doWith(func(string, string, any) (*http.Response, error) { return nil, want }, "GET", "/", nil, nil, "read")
		require.ErrorIs(t, err, want)
	})
}
