package clientcli

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestConsoleCommandPrintsURLAndOpensOnlyInteractiveMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		desktop bool
		openErr error
		opens   int
	}{
		{name: "desktop", desktop: true, opens: 1},
		{name: "print", args: []string{"--print"}, desktop: true},
		{name: "alias", args: []string{"--no-open"}, desktop: true},
		{name: "json", args: []string{"-o", "json"}, desktop: true},
		{name: "yaml", args: []string{"-o", "yaml"}, desktop: true},
		{name: "headless"},
		{name: "failed opener", desktop: true, opens: 1, openErr: errors.New("no browser")},
	} {
		t.Run(test.name, func(t *testing.T) {
			const consoleURL = "https://router.example/console/#overview"
			var stdout, stderr bytes.Buffer
			opened := 0
			root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
			root.PersistentFlags().StringP("output", "o", "", "format")
			root.AddCommand(newConsoleCmd(func(context.Context) (string, error) { return consoleURL, nil }, func(got string) error { opened++; require.Equal(t, consoleURL, got); return test.openErr }, func() bool { return test.desktop }))
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"console"}, test.args...))
			err := root.Execute()
			if test.openErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "printed URL")
			}
			require.Equal(t, test.opens, opened)
			require.Contains(t, stdout.String(), consoleURL)
			if test.name == "json" {
				require.JSONEq(t, `{"url":"`+consoleURL+`"}`, stdout.String())
			}
			if test.name == "yaml" {
				require.Contains(t, stdout.String(), "url:")
			}
		})
	}
}

func TestConsoleCommandIsRegistered(t *testing.T) {
	cmd, _, err := newRootCmd().Find([]string{"console"})
	require.NoError(t, err)
	require.Equal(t, "console", cmd.Name())
}

func TestConsoleCommandUsesConfiguredTLSClientWithoutHealthPreflight(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/.well-known/oauth-authorization-server", r.URL.Path)
		_, _ = w.Write([]byte(`{"issuer":"https://public-router.example"}`))
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "synthetic-ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	cm := config.NewConfigManager("zzrouter")
	require.NoError(t, cm.SaveClientConfig(&config.ClientConfig{Node: config.NodeConnection{Address: host, Port: port, Secure: true, TLSCACert: caPath}}))
	cmd := NewConsoleCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--print"})
	require.NoError(t, cmd.Execute())
	require.Equal(t, "https://public-router.example/console/#overview\n", output.String())
}
