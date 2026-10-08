package mesh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveClusterURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		public  string
		port    int
		want    string
		wantErr bool
	}{
		{
			name:   "http host+port swap to https+cluster port",
			public: "http://worker.example:9090",
			port:   9091,
			want:   "https://worker.example:9091",
		},
		{
			name:   "https host+port also rewritten",
			public: "https://worker.example:9443",
			port:   9091,
			want:   "https://worker.example:9091",
		},
		{
			name:   "IPv4 host preserved",
			public: "http://192.0.2.10:9090",
			port:   9091,
			want:   "https://192.0.2.10:9091",
		},
		{
			name:   "IPv6 host wrapped with brackets",
			public: "http://[fd00::1]:9090",
			port:   9091,
			want:   "https://[fd00::1]:9091",
		},
		{
			name:   "path preserved",
			public: "http://worker.example:9090/prefix",
			port:   9091,
			want:   "https://worker.example:9091/prefix",
		},
		{
			name:    "zero port rejected",
			public:  "http://worker.example:9090",
			port:    0,
			wantErr: true,
		},
		{
			name:    "negative port rejected",
			public:  "http://worker.example:9090",
			port:    -1,
			wantErr: true,
		},
		{
			name:    "missing host rejected",
			public:  "http:///just-a-path",
			port:    9091,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DeriveClusterURL(tc.public, tc.port)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
