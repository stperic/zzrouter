package httperr_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/httperr"
)

func TestSetRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string // empty means the header must be absent
	}{
		{"zero writes nothing", 0, ""},
		{"negative writes nothing", -time.Second, ""},
		{"one nanosecond rounds up to one second", 1, "1"},
		{"1500ms rounds up to two seconds", 1500 * time.Millisecond, "2"},
		{"whole seconds are unchanged", 5 * time.Second, "5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			httperr.SetRetryAfter(h, tt.d)
			if tt.want == "" {
				assert.Empty(t, h.Values("Retry-After"))
				return
			}
			assert.Equal(t, tt.want, h.Get("Retry-After"))
		})
	}
}
