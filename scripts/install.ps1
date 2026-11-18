# zzrouter Windows installer
# Usage (from the extracted release folder):
#   Admin (machine-wide):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#   Non-admin (per-user, default when not elevated):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#   As a cluster worker reachable on the LAN (admin):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1 -Role worker -Service
#
# What it does:
#   1. Copies zzrouter.exe, zzrouter-node.exe, zzrouter-launcher.exe to $InstallDir
#      - admin:     C:\Program Files\zzrouter
#      - per-user:  %LOCALAPPDATA%\Programs\zzrouter
#   2. Adds $InstallDir to PATH (Machine if admin, else User)
#   3. Writes a minimal node.yaml at %APPDATA%\zzrouter\node.yaml
#      (bind 127.0.0.1:9090, cluster.mode from -Role, default coordinator)
#   4. Creates inbound firewall rules for $Port and, when clustered,
#      $ClusterPort (admin only; skipped otherwise)
#   5. Starts zzrouter-node.exe in the background and verifies /health

[CmdletBinding()]
param(
    [string]$InstallDir,
    [string]$Bind = "127.0.0.1",
    [int]$Port = 9090,
    [ValidateSet("disabled", "worker", "coordinator")]
    [string]$Role = "coordinator",
    [int]$ClusterPort = 9091,
    [switch]$SkipStart,
    [switch]$PerUser,
    [switch]$Service,
    [switch]$NoService,
    [switch]$ForceConfig,
    [switch]$NoOpenConsole
)

# -Role picks what this node is. A worker still has to pair before it
# joins anything, but pairing refuses to run unless the mode is already
# worker, so installing as "disabled" means hand-editing YAML before the
# first useful command. -ClusterPort is the mTLS listener a coordinator
# and its workers speak on; it has to match across the cluster, because
# the coordinator reaches each worker by putting its own cluster port
# onto the worker's host.

# Run elevated, this installs the Windows service by DEFAULT, because a
# machine-wide install should leave something the machine keeps running.
# -NoService opts out and starts a detached node instead.
#
# Detached is the weaker guarantee: it dies with the session that launched
# it (an SSH session kills its whole job object on disconnect) and does
# not come back after a reboot. A non-elevated install cannot register a
# service at all, so it is always detached.
#
# It also moves the config to a machine-wide directory and points
# ZZROUTER_CONFIG_DIR at it for the whole machine, so the service account
# and the operator's CLI read ONE node.yaml. Without that the service
# resolves config under its own profile, silently runs a freshly
# defaulted config, and every later command disagrees with it.

# $Bind controls management access and defaults to 127.0.0.1.
# Cluster connections use a separate listener and firewall rule, so the
# console can stay local-only when adding workers.

$ErrorActionPreference = "Stop"

# Only this machine can reach a node bound to loopback. It decides the
# firewall and whether inference may be answered without a key.
$IsLoopback = ($Bind -eq "127.0.0.1" -or $Bind -eq "::1" -or $Bind -eq "localhost")
$RequireCompatAuth = "true"

# Get-ReachableAddress returns an address another machine can actually
# use to reach this one. $Bind may be 0.0.0.0, which is a listen
# wildcard, not a destination -- printing it sends operators to configure
# a coordinator with an address that can never connect.
function Get-ReachableAddress {
    if ($Bind -ne "0.0.0.0" -and $Bind -ne "::") { return $Bind }

    # Ask which interface carries the default route rather than ranking
    # all of them. A box with a VPN, a hypervisor switch or a second NIC
    # answers on several routable-looking addresses, and the wrong one
    # looks perfectly valid while being unreachable from the coordinator.
    $route = Get-NetRoute -DestinationPrefix "0.0.0.0/0" -ErrorAction SilentlyContinue |
        Sort-Object -Property RouteMetric, InterfaceMetric |
        Select-Object -First 1
    if ($route) {
        $addr = Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex $route.InterfaceIndex -ErrorAction SilentlyContinue |
            Where-Object { $_.IPAddress -ne "127.0.0.1" -and $_.IPAddress -notlike "169.254.*" } |
            Select-Object -First 1
        if ($addr) { return $addr.IPAddress }
    }
    return $env:COMPUTERNAME
}

function Write-Step($msg)  { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Ok($msg)    { Write-Host "    OK  $msg" -ForegroundColor Green }
function Write-Warn2($msg) { Write-Host "    !!  $msg" -ForegroundColor Yellow }
function Write-Err($msg)   { Write-Host "    XX  $msg" -ForegroundColor Red }

function Get-ConsoleShortcutSpec {
    param(
        [string]$InstallDir,
        [string]$ConfigDir,
        [string]$SystemDirectory = [Environment]::GetFolderPath("System")
    )
    $clientPath = Join-Path $InstallDir "zzrouter.exe"
    # PowerShell literals preserve spaces, apostrophes, dollar signs and
    # backticks in paths. Keep the window open so the one-time code is usable.
    $command = '$env:ZZROUTER_CONFIG_DIR = ''{0}''; & ''{1}'' console' -f $ConfigDir.Replace("'", "''"), $clientPath.Replace("'", "''")
    [pscustomobject]@{
        TargetPath = Join-Path $SystemDirectory "WindowsPowerShell\v1.0\powershell.exe"
        Arguments = '-NoProfile -NoExit -Command "{0}"' -f $command
        WorkingDirectory = $ConfigDir
        IconLocation = "$clientPath,0"
    }
}

# --- Windows version check ----------------------------------------------
# Minimum: Windows 10 1809 (build 17763) / Server 2019.
# Go 1.26 runtime and gopsutil v4 require this; older builds may
# malfunction silently.
$osBuild = [Environment]::OSVersion.Version.Build
if ($osBuild -lt 17763) {
    Write-Err "Windows 10 version 1809 (build 17763) or later is required."
    Write-Err "Current build: $osBuild. Please update Windows before installing."
    exit 1
}

# --- Admin detection (non-fatal) ----------------------------------------
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
$IsAdmin = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

# Default install dir depends on privilege
if (-not $InstallDir) {
    if ($IsAdmin -and -not $PerUser) {
        $InstallDir = "C:\Program Files\zzrouter"
    } else {
        $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\zzrouter"
    }
}
# -PerUser forces per-user even when elevated
if ($PerUser) { $IsAdmin = $false }

# Elevated installs get the service unless told otherwise. A non-elevated
# install cannot register one, so it stays detached regardless.
if ($IsAdmin -and -not $NoService) { $Service = $true }
if ($NoService) { $Service = $false }
if ($Service -and -not $IsAdmin) {
    Write-Err "-Service installs a Windows service and needs an elevated shell."
    exit 1
}

if ($IsAdmin) {
    Write-Step "Running as Administrator -- machine-wide install"
} else {
    Write-Step "Running without elevation -- per-user install"
    Write-Warn2 "Firewall rule and machine PATH update will be skipped."
}

# --- Locate bundle (the folder this script lives in) --------------------
$BundleDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Write-Step "Release bundle: $BundleDir"

$binaries = @("zzrouter.exe", "zzrouter-node.exe", "zzrouter-launcher.exe")
# In release bundles the binaries sit next to install.ps1. For local dev the
# script lives in scripts/ while the built binaries sit at the repo root, so
# fall back to the parent directory if the bundle dir is missing them.
$SourceDir = $BundleDir
$missing = $binaries | Where-Object { -not (Test-Path (Join-Path $BundleDir $_)) }
if ($missing) {
    $parent = Split-Path -Parent $BundleDir
    $missingInParent = $binaries | Where-Object { -not (Test-Path (Join-Path $parent $_)) }
    if (-not $missingInParent) {
        Write-Warn2 "Binaries not found next to install.ps1; using parent dir: $parent"
        $SourceDir = $parent
    } else {
        Write-Err "Missing binaries in bundle dir: $($missing -join ', ')"
        exit 1
    }
}
Write-Ok "All three binaries present."

# Resolve preserved configuration with the binary's parser before stopping or
# replacing anything. TLS upgrades need the existing authenticated updater.
if ($env:ZZROUTER_CONFIG_DIR) {
    if (-not [IO.Path]::IsPathRooted($env:ZZROUTER_CONFIG_DIR) -or $env:ZZROUTER_CONFIG_DIR -match '[\x00-\x1F]') { throw "ZZROUTER_CONFIG_DIR must be an absolute path without control characters." }
    $ConfigDir = [IO.Path]::GetFullPath($env:ZZROUTER_CONFIG_DIR)
}
elseif ($Service) { $ConfigDir = Join-Path $env:ProgramData "zzrouter" }
else { $ConfigDir = Join-Path $env:APPDATA "zzrouter" }
$ConfigFile = Join-Path $ConfigDir "node.yaml"
# The existing SCM account may differ from the default LocalSystem account.
# Check before stopping it or replacing artifacts; credential ACL changes must
# never exclude the account that actually runs the daemon.
& (Join-Path $SourceDir "zzrouter-node.exe") config credentials --dir $ConfigDir --check
if ($LASTEXITCODE -ne 0) { throw "The existing service account needs manual credential-permission review. No files or services were changed." }
$existingIdentity = $null
if ((Test-Path $ConfigFile) -and -not $ForceConfig) {
    $identityOutput = & (Join-Path $SourceDir "zzrouter-node.exe") config identity --dir $ConfigDir --json
    if ($LASTEXITCODE -ne 0) { throw "Could not read the existing installation. No files or services were changed." }
    $existingIdentity = ($identityOutput -join "`n") | ConvertFrom-Json
    if ($existingIdentity.secure) { throw "This installation uses HTTPS. Use its authenticated updater or configure the correct TLS trust before reinstalling. No files or services were changed." }
    if (-not $existingIdentity.mode -or -not $existingIdentity.bind -or -not $existingIdentity.port -or -not $existingIdentity.cluster_port) { throw "Incomplete installation identity. No files or services were changed." }
    $Role = $existingIdentity.mode
    $Bind = $existingIdentity.bind
    $Port = [int]$existingIdentity.port
    $ClusterPort = [int]$existingIdentity.cluster_port
    $IsLoopback = ($Bind -eq "127.0.0.1" -or $Bind -eq "::1" -or $Bind -eq "localhost")
}

# --- Stop any running node ----------------------------------------------
# This runs BEFORE the binaries are copied, and that ordering is the whole
# point: Windows will not overwrite a .exe that a running process has
# open, so copying first fails every upgrade over a live node with an
# IOException naming a file the operator did not know was in use.
#
# $existingSvc is set here and read again at the end, where the summary
# needs to know whether a service was already present.
$existingSvc = Get-Service -Name "zzrouter" -ErrorAction SilentlyContinue

function Stop-RunningNode {
# Stop any previous instance using the canonical CLI command, which matches
# what 'zzrouter-node stop' does. Fall back to Stop-Process if the binary
# isn't on PATH yet (fresh install over a manually-started instance).
# A previously installed Windows service has to be stopped through the
# SCM. Terminating its process only has the SCM start it again, and the
# node then keeps running the old binary and the service account's
# config, which reads as an install that silently did not take.
if ($existingSvc -and $existingSvc.Status -ne "Stopped") {
    if ($IsAdmin) {
        Write-Warn2 "Stopping the existing 'zzrouter' service"
        Stop-Service -Name "zzrouter" -Force
        $existingSvc.WaitForStatus("Stopped", [TimeSpan]::FromSeconds(20))
        Write-Ok "Service stopped."
    } else {
        Write-Err "The 'zzrouter' service is running and stopping it needs elevation."
        Write-Err "Re-run this installer as administrator."
        exit 2
    }
}

$existingProcs = Get-Process -Name "zzrouter-node" -ErrorAction SilentlyContinue
if ($existingProcs) {
    Write-Warn2 "Stopping previous zzrouter-node (PID $($existingProcs[0].Id))"
    $stopExe = $null
    # Prefer the already-installed binary (may be from a prior install)
    if (Test-Path (Join-Path $InstallDir "zzrouter-node.exe")) {
        $stopExe = Join-Path $InstallDir "zzrouter-node.exe"
    } elseif (Get-Command "zzrouter-node" -ErrorAction SilentlyContinue) {
        $stopExe = "zzrouter-node"
    }
    if ($stopExe) {
        try {
            & $stopExe stop 2>$null
            # Wait for process to exit
            for ($w = 0; $w -lt 8; $w++) {
                Start-Sleep -Milliseconds 500
                if (-not (Get-Process -Name "zzrouter-node" -ErrorAction SilentlyContinue)) { break }
            }
        } catch {
            # Fall through to force kill
        }
    }
    # Force kill anything still running
    Get-Process -Name "zzrouter-node" -ErrorAction SilentlyContinue | ForEach-Object {
        Write-Warn2 "Force-stopping zzrouter-node (PID $($_.Id))"
        $_ | Stop-Process -Force
        Start-Sleep -Seconds 1
    }
}
}

Stop-RunningNode

# --- Install binaries ---------------------------------------------------
Write-Step "Installing to: $InstallDir"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}
foreach ($b in $binaries) {
    Copy-Item -Path (Join-Path $SourceDir $b) -Destination (Join-Path $InstallDir $b) -Force
    Write-Ok "Copied $b"
}

# --- Add to PATH (idempotent) -------------------------------------------
$pathScope = if ($IsAdmin) { "Machine" } else { "User" }
Write-Step "Updating $pathScope PATH"
$existingPath = [Environment]::GetEnvironmentVariable("Path", $pathScope)
if ($existingPath -notlike "*$InstallDir*") {
    $newPath = if ([string]::IsNullOrEmpty($existingPath)) { $InstallDir } else { "$existingPath;$InstallDir" }
    [Environment]::SetEnvironmentVariable("Path", $newPath, $pathScope)
    Write-Ok "Added $InstallDir to $pathScope PATH (restart shells to pick up)."
} else {
    Write-Ok "$pathScope PATH already contains $InstallDir."
}
# Also make it visible to this session
$env:Path = "$env:Path;$InstallDir"

# --- Write minimal node.yaml -------------------------------------------
# The directory was resolved before stopping the old node. Pass the same
# explicit directory to the CLI and service; do not use LocalSystem's profile.
if ($Service) {
    # Machine scope so the service account sees it too; the process copy
    # is set as well so the health/identity checks below and any command
    # run from this shell resolve the same root without a new session.
    Write-Step "Pointing ZZROUTER_CONFIG_DIR at $ConfigDir (machine-wide)"
    [Environment]::SetEnvironmentVariable("ZZROUTER_CONFIG_DIR", $ConfigDir, "Machine")
    $env:ZZROUTER_CONFIG_DIR = $ConfigDir
    Write-Ok "Service and CLI will both read $ConfigFile"
}
$env:ZZROUTER_CONFIG_DIR = $ConfigDir

Write-Step "Writing config: $ConfigFile"
if (-not (Test-Path $ConfigDir)) {
    New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null
}

$nodeYaml = @"
# Minimal zzrouter node.yaml written by install.ps1
# Defaults to loopback for single-machine use. To expose this node on the LAN
# for multi-node setups, choose -Bind 0.0.0.0 on first install. Existing
# installations preserve this setting; edit it deliberately before restart.
node:
  name: ""
  bind: "$Bind"
  port: $Port

cluster:
  mode: $Role
  bind_port: $ClusterPort

# auth keys are env-var-name references (resolved at request time).
# The validator in validateNodeConfig() just checks these are non-empty.
# The cluster key is provisioned by the coordinator during the join handshake.
auth:
  admin_key: "ZZROUTER_ADMIN_API_KEY"
  user_key: "ZZROUTER_API_KEY"
  # Applications use named keys whose access and budgets come from their team.
  require_compat_auth: $RequireCompatAuth
"@

# An existing config is left alone. It is not ours to rewrite: a node that
# has been running owns settings this template does not carry --
# advertise_ips, the coordinator URL and CA fingerprint it paired with,
# observability, update policy -- and regenerating the file would silently
# drop every one of them. An installer that eats the config on upgrade is
# worse than one that refuses to.
#
# -ForceConfig overwrites (after a backup) for the case where the operator
# really is re-provisioning the node.
if ((Test-Path $ConfigFile) -and -not $ForceConfig) {
    Write-Ok "Keeping existing $ConfigFile (cluster.mode=$Role)"
    Write-Host "    Pass -ForceConfig to regenerate it from the installer template."
} else {
    if (Test-Path $ConfigFile) {
        $backup = "$ConfigFile.bak.$(Get-Date -Format yyyyMMdd-HHmmss)"
        Copy-Item $ConfigFile $backup -Force
        Write-Warn2 "Existing node.yaml backed up to $backup"
    }
    # Write without BOM — PowerShell 5.1's -Encoding UTF8 emits a BOM which
    # can confuse non-Go YAML parsers. Same pattern as the .env write below.
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($ConfigFile, $nodeYaml, $utf8NoBom)
    Write-Ok "Wrote node.yaml (bind ${Bind}:$Port, cluster.mode=$Role, cluster.bind_port=$ClusterPort)"
}

# Credential creation, preservation and ACLs use the same Go implementation
# as service setup. Never print credentials or generate them in PowerShell.
$nodeExe = Join-Path $InstallDir "zzrouter-node.exe"
$credentialArgs = @("config", "credentials", "--dir", $ConfigDir)
$legacyFile = Join-Path $ConfigDir "zzrouter.env"
if (Test-Path $legacyFile) { $credentialArgs += @("--legacy-file", $legacyFile) }
& $nodeExe @credentialArgs
if ($LASTEXITCODE -ne 0) { throw "Could not prepare installation credentials. Existing configuration was preserved." }

# A stable entry point lets people return without finding the installation folder.
$programs = [Environment]::GetFolderPath("Programs")
$shortcutDir = Join-Path $programs "zzRouter"
New-Item -ItemType Directory -Path $shortcutDir -Force | Out-Null
$shell = New-Object -ComObject WScript.Shell
$shortcutPath = Join-Path $shortcutDir "Router console.lnk"
$shortcut = $shell.CreateShortcut($shortcutPath)
$shortcutSpec = Get-ConsoleShortcutSpec -InstallDir $InstallDir -ConfigDir $ConfigDir
$shortcut.TargetPath = $shortcutSpec.TargetPath
$shortcut.Arguments = $shortcutSpec.Arguments
$shortcut.WorkingDirectory = $shortcutSpec.WorkingDirectory
$shortcut.IconLocation = $shortcutSpec.IconLocation
$shortcut.Description = "Open your zzRouter console"
$shortcut.Save()

# --- Firewall rules (management and cluster listeners, admin only) -----
# Both ports, not just the admin one. A cluster node that only opens
# $Port pairs successfully and then sits at health "unknown" forever,
# because every coordinator-to-worker call goes to the mTLS listener on
# $ClusterPort instead.
$FirewallPorts = @()
if (-not $IsLoopback) { $FirewallPorts += $Port }
if ($Role -ne "disabled") { $FirewallPorts += $ClusterPort }

if ($FirewallPorts.Count -eq 0) {
    Write-Step "Firewall rules: no network listeners to open"
} elseif ($IsAdmin) {
    foreach ($fwPort in $FirewallPorts) {
        Write-Step "Firewall rule for TCP $fwPort"
        $ruleName = "zzrouter $fwPort"
        $existing = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
        if (-not $existing) {
            New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Protocol TCP `
                -LocalPort $fwPort -Action Allow -Profile Any | Out-Null
            Write-Ok "Created firewall rule '$ruleName'."
        } else {
            Write-Ok "Firewall rule '$ruleName' already exists."
        }
    }
} else {
    Write-Warn2 "Skipping firewall rules (non-admin). Windows may prompt on first listen, or block LAN access."
}

# --- Start node ---------------------------------------------------------
if ($SkipStart) {
    Write-Warn2 "SkipStart flag set -- not starting zzrouter-node."
    exit 0
}

if ($Service) {
    Write-Step "Installing and starting the zzrouter Windows service"
} else {
    Write-Step "Starting zzrouter-node (detached)"
}
$nodeExe = Join-Path $InstallDir "zzrouter-node.exe"


# Two ways to bring the node up, and they are not interchangeable.
#
# The service (elevated `start` with no --no-setup) is registered with the
# SCM, so it outlives the shell that installed it and comes back after a
# reboot. A detached node is a child of this shell: DETACHED_PROCESS |
# CREATE_NEW_PROCESS_GROUP (see process_windows.go) frees it from the
# console, but not from a job object, and an SSH session kills its job on
# disconnect. That is why a remote install without -Service leaves nothing
# running once the connection drops.
if ($Service) {
    & $nodeExe start --dir $ConfigDir
    if ($LASTEXITCODE -ne 0) {
        Write-Err "zzrouter-node start (service install) exited with code $LASTEXITCODE"
        exit 2
    }
    Write-Ok "Windows service installed and started"
} else {
    & $nodeExe start --no-setup --detach --dir $ConfigDir
    if ($LASTEXITCODE -ne 0) {
        Write-Err "zzrouter-node start --detach exited with code $LASTEXITCODE"
        exit 2
    }
    Write-Ok "Launcher returned; detached server should now be running"
}

# --- Health check -------------------------------------------------------
Write-Step "Waiting for /health on 127.0.0.1:$Port"
$healthy = $false
for ($i = 1; $i -le 20; $i++) {
    Start-Sleep -Seconds 1
    try {
        $r = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/health" -UseBasicParsing -TimeoutSec 2
        if ($r.StatusCode -eq 200) {
            $healthy = $true
            Write-Ok "Health check passed (HTTP 200) after ${i}s"
            Write-Host "    body: $($r.Content)"
            break
        }
    } catch {
        # retry
    }
}

if (-not $healthy) {
    Write-Err "zzrouter-node did not become healthy within 20s."
    # Two log directories, because a detached node and a node run as
    # the Windows service do not write to the same one: the service
    # logs under ProgramData, a detached node under this account's
    # LOCALAPPDATA. Looking in only one is how a startup failure ends
    # up with no log at all to read.
    $srvLogDirs = @(
        (Join-Path $env:LOCALAPPDATA "zzrouter\logs"),
        (Join-Path $env:PROGRAMDATA "zzrouter\logs")
    )
    foreach ($srvLogDir in $srvLogDirs) {
        Write-Host "    Check logs under: $srvLogDir"
        if (Test-Path $srvLogDir) {
            Get-ChildItem $srvLogDir -File | ForEach-Object {
                Write-Host "--- tail $($_.FullName) ---"
                Get-Content $_.FullName -Tail 20
            }
        }
    }
    Get-Process zzrouter-node -ErrorAction SilentlyContinue |
        Format-Table Id,ProcessName,StartTime | Out-Host
    exit 2
}

# --- Identity check -----------------------------------------------------
# A 200 on /health only proves something is listening, not that it is the
# node this script just configured. The two diverge for real: an elevated
# start that installs the Windows service leaves a LocalSystem process
# serving this port from the service account's node.yaml, which answers
# /health perfectly while ignoring every setting written above. Ask the
# server which config it loaded and refuse to report success if it is not
# ours. config_path is loopback-only, and this probe is on 127.0.0.1.
Write-Step "Verifying the running node loaded $ConfigFile"
$expectedVersion = ((& $nodeExe version | Select-Object -First 1) -replace '^zzrouter-node version ', '').Trim()
if ($LASTEXITCODE -ne 0 -or -not $expectedVersion) {
    Write-Err "Could not read the installed bundle version."
    exit 2
}
try {
    $detail = (Invoke-WebRequest -Uri "http://127.0.0.1:$Port/health?detailed=true" -UseBasicParsing -TimeoutSec 5).Content | ConvertFrom-Json
    $ident = $detail.details.node_identity
} catch {
    $ident = $null
}

if (-not $ident) {
    Write-Err "Could not verify which installation is running. Check zzrouter-node status before continuing."
    exit 2
} else {
    $loaded = $ident.config_path
    $mode   = $ident.cluster_mode
    if (-not $loaded -or ($loaded -ne $ConfigFile)) {
        Write-Err "The server on port $Port is NOT running the config this script wrote."
        Write-Host "    wrote  : $ConfigFile"
        Write-Host "    loaded : $loaded"
        Write-Host "    mode   : $mode (expected $Role)"
        Write-Host ""
        Write-Host "    That is another zzrouter reading a different config, almost"
        Write-Host "    always the Windows service running as its own account."
        Write-Host "    Stop and remove it, then re-run this installer:"
        Write-Host "      sc.exe stop zzrouter"
        Write-Host "      sc.exe delete zzrouter"
        exit 2
    }
    if ($mode -ne $Role) {
        Write-Err "The running node reports cluster mode '$mode' but was installed as '$Role'."
        Write-Host "    config : $loaded"
        exit 2
    }
    if ($detail.version -ne $expectedVersion) {
        Write-Err "The running node version '$($detail.version)' does not match the installed bundle '$expectedVersion'."
        exit 2
    }
    Write-Ok "Running $expectedVersion, loaded $loaded (cluster mode: $mode)"
}
if ($Role -ne "worker") {
    Write-Host "Open your router: zzrouter console" -ForegroundColor Cyan
    if (-not $NoOpenConsole -and [Environment]::UserInteractive) {
        try { Start-Process -FilePath $shortcutPath }
        catch { Write-Warn2 "Open Router console from the Start menu, or run: zzrouter console" }
    }
}

Write-Host ""
if ($existingSvc -and -not $Service) {
    Write-Warn2 "A 'zzrouter' Windows service is installed on this machine."
    Write-Warn2 "It was stopped above, and this install started a detached node instead."
    Write-Warn2 "The two cannot both run, and the service reads a DIFFERENT node.yaml"
    Write-Warn2 "(the service account's, not $ConfigFile)."
    Write-Warn2 "Pick one: re-run elevated (installs the service properly), or"
    Write-Warn2 "'sc.exe config zzrouter start= demand' to stop it coming back at boot."
    Write-Host ""
}
if (-not $Service) {
    # Said plainly because the failure is otherwise a mystery: the install
    # reports healthy, the SSH session ends, and the node is gone.
    Write-Warn2 "This node was started detached, so it does NOT survive a reboot,"
    Write-Warn2 "and it is killed when the session that launched it ends (an SSH"
    Write-Warn2 "session takes its child processes with it on disconnect)."
    Write-Warn2 "For a permanent node, re-run this installer from an elevated shell."
    Write-Host ""
}
$nodeProc = Get-Process zzrouter-node -ErrorAction SilentlyContinue | Select-Object -First 1
Write-Host "zzrouter-node is running." -ForegroundColor Green
Write-Host "  Install dir : $InstallDir"
Write-Host "  Config file : $ConfigFile"
Write-Host "  Listening   : ${Bind}:$Port"
if ($Service) {
    Write-Host "  Run mode    : Windows service 'zzrouter' (survives logoff and reboot)"
} else {
    Write-Host "  Run mode    : detached process (ends with this session)"
}
if ($nodeProc) { Write-Host ("  PID         : {0}" -f $nodeProc.Id) }
Write-Host ""
if ($IsLoopback) {
    Write-Host "Console access stays on this computer. Worker connections use cluster port $ClusterPort." -ForegroundColor Cyan
}
if ($Role -eq "worker") {
    Write-Host "Next: return to Nodes > Add compute node in your coordinator console." -ForegroundColor Cyan
    Write-Host "Copy its pairing command, run it here, then return the pairing details for approval."
    Write-Host "Keep TCP $ClusterPort reachable from your coordinator."
} elseif ($Role -eq "coordinator") {
    Write-Host "Your coordinator is ready. Workers are optional." -ForegroundColor Cyan
    Write-Host "When ready, open Nodes > Add compute node for installation and pairing instructions."
} else {
    Write-Host "Clustering is disabled. Use the console for this computer." -ForegroundColor Cyan
}
