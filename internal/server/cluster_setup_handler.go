package server

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/version"
)

// Installation instructions are an API projection, not executable downloads.
type workerInstallGuide struct {
	OS            string               `json:"os"`
	Label         string               `json:"label"`
	Archives      string               `json:"archives"`
	Installations []workerInstallation `json:"installations"`
}

type workerInstallation struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Shell          string `json:"shell"`
	Startup        string `json:"startup"`
	InstallCommand string `json:"install_command"`
	PairCommand    string `json:"pair_command"`
	PairShell      string `json:"pair_shell,omitempty"`
}

type workerSetupGuide struct {
	CoordinatorURL string               `json:"coordinator_url"`
	CAFingerprint  string               `json:"ca_fingerprint"`
	DownloadURL    string               `json:"download_url"`
	Version        string               `json:"version"`
	WorkerPort     int                  `json:"worker_port"`
	Platforms      []workerInstallGuide `json:"platforms"`
}

func (h *ClusterHandlers) handleWorkerSetup(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "The cluster listener is not ready. Start the coordinator and try again.")
		return
	}
	endpoint, err := clusternode.CoordinatorSelfCoordURL(h.listener, h.advertiseURL)
	if err != nil {
		ServiceUnavailable(c, "The coordinator cluster address is unavailable: "+err.Error())
		return
	}
	address, err := url.Parse(endpoint)
	if err != nil || address.Scheme != "https" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || address.Path != "" && address.Path != "/" {
		BadRequest(c, "Configure cluster.advertise_url as the reachable HTTPS cluster address, without a path or credentials.")
		return
	}
	ip := net.ParseIP(address.Hostname())
	if address.Hostname() == "localhost" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		BadRequest(c, "The cluster address is local-only. Configure the cluster listener and cluster.advertise_url for your LAN or VPN. The console can remain local-only.")
		return
	}
	port := h.config.Cluster.BindPort
	if port == 0 {
		port = constants.DefaultClusterPort
	}
	respondRetrievalSuccess(c, "Add a compute node", buildWorkerSetupGuide(endpoint, h.listener.CAFingerprint(), port, version.Current))
}

func buildWorkerSetupGuide(endpoint, fingerprint string, workerPort int, release *version.Version) workerSetupGuide {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	powershellQuote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	args := " cluster pair --no-tui --secure --coordinator-url " + quote(endpoint) + " --ca-fingerprint " + quote(fingerprint)
	windowsArgs := " cluster pair --no-tui --secure --coordinator-url " + powershellQuote(endpoint) + " --ca-fingerprint " + powershellQuote(fingerprint)
	download := fmt.Sprintf("https://github.com/%s/%s/releases", update.DefaultOwner, update.DefaultRepo)
	if release.PreRelease != "dev" {
		tag := release.ShortString()
		if release.PreRelease != "" {
			tag += "-" + release.PreRelease
		}
		download += "/tag/v" + tag
	}
	return workerSetupGuide{
		CoordinatorURL: endpoint, CAFingerprint: fingerprint, DownloadURL: download, Version: release.String(), WorkerPort: workerPort,
		Platforms: []workerInstallGuide{
			{OS: "darwin", Label: "macOS", Archives: "darwin-arm64 for Apple silicon; darwin-amd64 for Intel", Installations: []workerInstallation{
				{ID: "user", Label: "This account", Shell: "Terminal, in the extracted release folder", Startup: "Starts when you log in",
					InstallCommand: fmt.Sprintf("bash ./install.sh --role worker --cluster-port %d --yes", workerPort), PairCommand: "\"$HOME/.local/bin/zzrouter-node\"" + args},
			}},
			{OS: "windows", Label: "Windows", Archives: "windows-amd64", Installations: []workerInstallation{
				{ID: "service", Label: "All users: starts at boot (recommended)", Shell: "PowerShell as Administrator, in the extracted release folder", Startup: "Windows service, starts at boot",
					InstallCommand: fmt.Sprintf("powershell -NoProfile -ExecutionPolicy Bypass -File .\\install.ps1 -Role worker -ClusterPort %d -Service", workerPort), PairCommand: "$env:ZZROUTER_CONFIG_DIR = Join-Path $env:PROGRAMDATA 'zzrouter'; zzrouter-node" + windowsArgs, PairShell: "Open a new administrator PowerShell window so it can find the installed command. Use the same account and installation type."},
				{ID: "user", Label: "This account: current session only", Shell: "PowerShell, in the extracted release folder", Startup: "Runs for this session. An administrator may need to allow the cluster port through Windows Firewall.",
					InstallCommand: fmt.Sprintf("powershell -NoProfile -ExecutionPolicy Bypass -File .\\install.ps1 -Role worker -ClusterPort %d -PerUser -NoService", workerPort), PairCommand: "$env:ZZROUTER_CONFIG_DIR = Join-Path $env:APPDATA 'zzrouter'; zzrouter-node" + windowsArgs, PairShell: "Open a new PowerShell window so it can find the installed command. Use the same account and installation type."},
			}},
			{OS: "linux", Label: "Linux", Archives: "linux-amd64 for Intel/AMD; linux-arm64 for ARM", Installations: []workerInstallation{
				{ID: "service", Label: "System service", Shell: "Terminal, in the extracted release folder", Startup: "systemd service, starts at boot",
					InstallCommand: fmt.Sprintf("sudo bash ./install.sh --role worker --cluster-port %d --yes", workerPort), PairCommand: "sudo env ZZROUTER_CONFIG_DIR=/etc/zzrouter /usr/local/bin/zzrouter-node" + args},
			}},
		},
	}
}
