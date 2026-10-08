package keepalive

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	type want struct {
		keepAliveSet bool
		keepAlive    time.Duration
	}

	cases := []struct {
		name string
		body string
		want want
	}{
		{
			name: "no keep_alive present",
			body: `{"model":"llama3","prompt":"hi"}`,
			want: want{keepAliveSet: false},
		},
		{
			name: "keep_alive as duration string",
			body: `{"model":"x","keep_alive":"10m"}`,
			want: want{keepAliveSet: true, keepAlive: 10 * time.Minute},
		},
		{
			name: "keep_alive as compound duration string",
			body: `{"keep_alive":"1h30m"}`,
			want: want{keepAliveSet: true, keepAlive: 90 * time.Minute},
		},
		{
			name: "keep_alive as integer seconds",
			body: `{"keep_alive":300}`,
			want: want{keepAliveSet: true, keepAlive: 5 * time.Minute},
		},
		{
			name: "keep_alive 0 means unload promptly",
			body: `{"keep_alive":0}`,
			want: want{keepAliveSet: true, keepAlive: 0},
		},
		{
			name: "keep_alive -1 means indefinite (negative sentinel)",
			body: `{"keep_alive":-1}`,
			want: want{keepAliveSet: true, keepAlive: -1 * time.Second},
		},
		{
			name: "keep_alive large negative also maps to indefinite",
			body: `{"keep_alive":-999999}`,
			want: want{keepAliveSet: true, keepAlive: -1 * time.Second},
		},
		{
			name: "empty body returns zero override",
			body: ``,
			want: want{keepAliveSet: false},
		},
		{
			name: "malformed JSON returns zero override (tolerant)",
			body: `{not json`,
			want: want{keepAliveSet: false},
		},
		{
			name: "keep_alive with unparseable string silently ignored",
			body: `{"keep_alive":"not-a-duration"}`,
			want: want{keepAliveSet: false},
		},
		{
			name: "keep_alive with wrong type (bool) silently ignored",
			body: `{"keep_alive":true}`,
			want: want{keepAliveSet: false},
		},
		{
			name: "unrelated fields coexist",
			body: `{"model":"x","stream":true,"keep_alive":"5m","messages":[]}`,
			want: want{keepAliveSet: true, keepAlive: 5 * time.Minute},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Parse([]byte(tc.body))
			require.NotNil(t, o, "Parse must always return non-nil")

			if tc.want.keepAliveSet {
				require.NotNil(t, o.Duration, "Duration should be set")
				assert.Equal(t, tc.want.keepAlive, *o.Duration)
			} else {
				assert.Nil(t, o.Duration, "Duration should be nil when absent/malformed")
			}
		})
	}
}

func TestParse_NeverReturnsNil(t *testing.T) {
	// Contract: Parse always returns non-nil, even for pathological input.
	for _, body := range [][]byte{nil, {}, []byte(`null`), []byte(`garbage`), []byte(`[]`)} {
		o := Parse(body)
		assert.NotNil(t, o, "body=%q", string(body))
	}
}
