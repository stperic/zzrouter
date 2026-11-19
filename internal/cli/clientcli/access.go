package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"github.com/stperic/common/httpclient"
	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/security"
)

// NewAccessCmd owns explicit local operator account recovery, without resetting configuration.
func NewAccessCmd() *cobra.Command {
	var dir string
	root := &cobra.Command{Use: "access", Short: "Recover access to this installation", Long: "Recover the owner account on the installed coordinator computer.\nRun as the installation account or an administrator. Choose the new password in the console."}
	recover := &cobra.Command{Use: "recover-owner", Short: "Create a temporary owner password recovery code", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if dir == "" {
			dir = config.NewConfigManager("zzrouter").GetNodeConfigDir()
		}
		grant, err := recoverLocalOwner(cmd.Context(), dir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Open %s\nTemporary recovery code (expires %s):\n%s\nChoose 'Use a temporary code' to set a new owner password. Existing app keys and configuration are preserved.\n", grant.URL, grant.ExpiresAt.Local().Format("15:04"), grant.Token)
		return err
	}}
	recover.Flags().StringVar(&dir, "dir", "", "Installed configuration directory")
	root.AddCommand(recover)
	return root
}

type localRecoveryGrant struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func recoverLocalOwner(ctx context.Context, dir string) (*localRecoveryGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	session, err := openLocal(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer session.client.CloseIdleConnections()
	req, err := session.request(ctx, http.MethodPost, "/zzrouter/v1/access/recovery/local", []byte("{}"))
	if err != nil {
		return nil, err
	}
	response, err := session.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact the running local installation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local recovery refused (%d): ensure the selected installation is running and has an owner account", response.StatusCode)
	}
	var result struct {
		Data localRecoveryGrant `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result); err != nil {
		return nil, fmt.Errorf("could not read the recovery response")
	}
	if result.Data.Token == "" || len(result.Data.Token) > 256 || result.Data.URL != session.file.ConsoleURL || result.Data.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("local recovery returned an invalid grant")
	}
	return &result.Data, nil
}

// localSession reaches the selected installation's running node as an operator on its host.
// Its client has no total deadline, so a long download can finish: every caller bounds
// its own request with a context deadline, as recoverLocalOwner does.
type localSession struct {
	file   *localproof.File
	client *http.Client // The only client that may send the proof: no proxy, no other origin.
}

// openLocal reads the installation's local proof and accepts only its node's literal local endpoint.
func openLocal(ctx context.Context, dir string) (*localSession, error) {
	file, err := localproof.Read(ctx, dir)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(file.Endpoint)
	if err != nil || !localproof.Address(endpoint.Hostname()) || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" || endpoint.Port() == "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("the installation endpoint must be the installed node's literal local address")
	}
	transport := &http.Transport{} // No environment proxy may receive the local proof.
	if endpoint.Scheme == "https" {
		transport.TLSClientConfig, err = security.TLSClientConfig(file.TLSCert)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig.ServerName = file.TLSName
	}
	return &localSession{file: file, client: httpclient.SameOrigin(&http.Client{Transport: transport})}, nil
}

// request builds a request to the installation's node that carries the local proof; send it with s.client.
func (s *localSession) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.file.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(localproof.Header, s.file.Proof)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
