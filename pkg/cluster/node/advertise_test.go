package clusternode

import (
	"context"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateAdvertiseURL pins the shared parse-validation helper.
// Single source of truth for the format check; Server Start
// (fail-fast) and the pairing accept handler (defense-in-depth)
// both call it.
func TestValidateAdvertiseURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"https://coord-01.internal:9091", false},
		{"http://127.0.0.1:9091", false},
		{"https://coord.example.com", false}, // port optional
		{"coord.example.com:9091", true},     // missing scheme
		{"https://", true},                   // missing host
		{"::::not-a-url", true},              // unparseable
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAdvertiseURL(tc.in)
			if tc.wantErr {
				assert.Error(t, err, "input %q", tc.in)
			} else {
				assert.NoError(t, err, "input %q", tc.in)
			}
		})
	}
}

func TestCoordinatorURLUsesClusterListenerAndNormalizesIntent(t *testing.T) {
	n := &Node{cfg: Config{BindHost: "0.0.0.0", NodeName: "coordinator", AdvertiseIPs: []net.IP{net.ParseIP("192.0.2.10")}}, listenerAddr: &net.TCPAddr{IP: net.IPv4zero, Port: 9191}}
	derived, err := CoordinatorSelfCoordURL(n, "")
	require.NoError(t, err)
	require.Equal(t, "https://192.0.2.10:9191", derived)
	for _, test := range []struct{ input, want string }{
		{"https://coord.example", "https://coord.example:9091"},
		{"https://coord.example:443/", "https://coord.example:443"},
		{"http://coord.example:9191/path?x=y", "https://coord.example:9191"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]:9091"},
		{"https://[2001:db8::1]:443/", "https://[2001:db8::1]:443"},
	} {
		t.Run(test.input, func(t *testing.T) {
			endpoint, err := CoordinatorSelfCoordURL(n, test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, endpoint)
			intent, err := NormalizeCoordinatorURL(endpoint)
			require.NoError(t, err)
			require.Equal(t, endpoint, intent)
		})
	}
	for _, invalid := range []string{"", "ftp://coord", "https://user:secret@coord", "https://coord:0", "https://coord:65536"} {
		_, err := NormalizeCoordinatorURL(invalid)
		require.Error(t, err, invalid)
	}
}

func TestCoordinatorURLUsesDetectedAddressesByDefault(t *testing.T) {
	cfg := testConfig(t, Coordinator)
	cfg.BindHost = "0.0.0.0"
	cfg.AdvertiseIPs = nil
	n, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, n.Start(t.Context()))
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	endpoint, err := CoordinatorSelfCoordURL(n, "")
	require.NoError(t, err)
	parsed, err := url.Parse(endpoint)
	require.NoError(t, err)
	require.Equal(t, ResolveSelfHost(cfg.BindHost, cfg.NodeName, cfg.advertiseIPs()), parsed.Hostname())
}

func TestEnrollmentHostFollowsCoordinatorRoute(t *testing.T) {
	for _, tc := range []struct {
		name, target, want string
		cfg                Config
		ips                []net.IP
	}{
		{"IPv4 despite IPv6 first", "https://127.0.0.1:9091", "127.0.0.1", Config{}, []net.IP{net.ParseIP("fd00::10"), net.ParseIP("127.0.0.1")}},
		{"IPv6 despite IPv4 first", "https://[::1]:9091", "::1", Config{}, []net.IP{net.ParseIP("192.0.2.10"), net.IPv6loopback}},
		{"explicit bind", "https://127.0.0.1:9091", "192.0.2.20", Config{BindHost: "192.0.2.20"}, []net.IP{net.ParseIP("127.0.0.1")}},
		{"explicit advertise order", "https://127.0.0.1:9091", "fd00::10", Config{AdvertiseIPs: []net.IP{net.ParseIP("fd00::10")}}, []net.IP{net.ParseIP("fd00::10"), net.ParseIP("127.0.0.1")}},
		{"route absent from certificate addresses", "https://127.0.0.1:9091", "192.0.2.10", Config{}, []net.IP{net.ParseIP("192.0.2.10")}},
		{"invalid target keeps fallback", "://invalid", "192.0.2.10", Config{}, []net.IP{net.ParseIP("192.0.2.10")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == "::1" {
				listener, err := net.ListenPacket("udp6", "[::1]:0")
				if err != nil {
					t.Skip("IPv6 loopback is unavailable")
				}
				_ = listener.Close()
			}
			require.Equal(t, tc.want, tc.cfg.enrollmentHost(t.Context(), tc.target, tc.ips))
		})
	}
}
