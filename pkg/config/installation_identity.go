package config

import (
	"context"
	"path/filepath"

	"github.com/stperic/zzrouter/pkg/constants"
)

// InstallationIdentity is the non-secret configuration needed to verify an upgrade.
type InstallationIdentity struct {
	Name        string      `json:"name"`
	Bind        string      `json:"bind"`
	Port        int         `json:"port"`
	Mode        ClusterMode `json:"mode"`
	ClusterPort int         `json:"cluster_port"`
	Secure      bool        `json:"secure"`
	ConfigPath  string      `json:"config_path"`
}

// ReadInstallationIdentity parses an existing installation without creating
// files, loading credentials into the environment or disclosing configuration.
func ReadInstallationIdentity(ctx context.Context, dir string) (*InstallationIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(dir, hostConfigFileName))
	if err != nil {
		return nil, err
	}
	cfg, err := readNodeConfigFile(path)
	if err != nil {
		return nil, err
	}
	identity := &InstallationIdentity{Name: cfg.Node.Name, Bind: cfg.Node.Bind, Port: cfg.Node.Port, Mode: cfg.Cluster.Mode, ClusterPort: cfg.Cluster.BindPort, Secure: cfg.Node.IsTLSEnabled(), ConfigPath: path}
	if identity.Port == 0 {
		identity.Port = constants.DefaultZZROUTERPort
	}
	if identity.Bind == "" {
		identity.Bind = "0.0.0.0"
	}
	if identity.Mode == "" {
		identity.Mode = ClusterModeDisabled
	}
	if identity.ClusterPort == 0 {
		identity.ClusterPort = constants.DefaultClusterPort
	}
	return identity, nil
}
