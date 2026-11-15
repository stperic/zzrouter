package shared

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserAvailability(t *testing.T) {
	for _, test := range []struct {
		name, platform string
		env            map[string]string
		want           bool
	}{
		{name: "mac", platform: "darwin", want: true},
		{name: "windows", platform: "windows", want: true},
		{name: "headless", platform: "linux"},
		{name: "display", platform: "linux", env: map[string]string{"DISPLAY": ":0"}, want: true},
		{name: "wayland", platform: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, want: true},
		{name: "remote mac", platform: "darwin", env: map[string]string{"SSH_CONNECTION": "present"}},
		{name: "forwarded display", platform: "linux", env: map[string]string{"SSH_TTY": "present", "DISPLAY": ":10"}},
		{name: "remote windows", platform: "windows", env: map[string]string{"SSH_CLIENT": "present"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, browserAvailable(test.platform, func(key string) string { return test.env[key] }))
		})
	}
}
