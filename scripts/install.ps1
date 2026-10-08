# zzrouter Windows installer
# Usage (from the extracted release folder):
#   Admin (machine-wide):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#   Non-admin (per-user, default when not elevated):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#   As a cluster worker reachable on the LAN (admin):
#     powershell -ExecutionPolicy Bypass -File .\install.ps1 -Bind 0.0.0.0 -Role worker
#
# What it does:
#   1. Copies zzrouter.exe, zzrouter-node.exe, zzrouter-launcher.exe to $InstallDir
#      - admin:     C:\Program Files\zzrouter
#      - per-user:  %LOCALAPPDATA%\Programs\zzrouter
#   2. Adds $InstallDir to PATH (Machine if admin, else User)
#   3. Writes a minimal node.yaml at %APPDATA%\zzrouter\node.yaml
#      (bind 0.0.0.0:9090, cluster.mode from -Role, default disabled)
#   4. Creates inbound firewall rules for $Port and, when clustered,
#      $ClusterPort (admin only; skipped otherwise)
#   5. Starts zzrouter-node.exe in the background and verifies /health

[CmdletBinding()]
param(
    [string]$InstallDir,
    [string]$Bind = "127.0.0.1",
    [int]$Port = 9090,
    [ValidateSet("disabled", "worker", "coordinator")]
    [string]$Role = "disabled",
    [int]$ClusterPort = 9091,
    [switch]$SkipStart,
    [switch]$PerUser,
    [switch]$Service,
    [switch]$NoService,
    [switch]$ForceConfig
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

# $Bind defaults to 127.0.0.1 (single-machine / try-it-out use case).
# Pass -Bind 0.0.0.0 to expose the node on the LAN for multi-node setups,
# together with -Role worker (or -Role coordinator) so the node comes up
# in the mode it is meant to run in.

$ErrorActionPreference = "Stop"

# Only this machine can reach a node bound to loopback. It decides the
# firewall and whether inference may be answered without a key.
$IsLoopback = ($Bind -eq "127.0.0.1" -or $Bind -eq "::1" -or $Bind -eq "localhost")
$RequireCompatAuth = if ($IsLoopback) { "false" } else { "true" }

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
# Note: zzrouter-node resolves config via kirsle/configdir.LocalConfig(),
# which on Windows maps to %APPDATA% (Roaming), NOT %LOCALAPPDATA%.
# A service runs as its own account, so that default would land somewhere
# the operator never edits -- hence the machine-wide directory plus
# ZZROUTER_CONFIG_DIR, which the node honours for the whole config root
# (node.yaml, providers/, .env, and the cluster ca/identity material).
if ($Service) {
    $ConfigDir = Join-Path $env:ProgramData "zzrouter"
} else {
    $ConfigDir = Join-Path $env:APPDATA "zzrouter"
}
$ConfigFile = Join-Path $ConfigDir "node.yaml"

if ($Service) {
    # Machine scope so the service account sees it too; the process copy
    # is set as well so the health/identity checks below and any command
    # run from this shell resolve the same root without a new session.
    Write-Step "Pointing ZZROUTER_CONFIG_DIR at $ConfigDir (machine-wide)"
    [Environment]::SetEnvironmentVariable("ZZROUTER_CONFIG_DIR", $ConfigDir, "Machine")
    $env:ZZROUTER_CONFIG_DIR = $ConfigDir
    Write-Ok "Service and CLI will both read $ConfigFile"
}

Write-Step "Writing config: $ConfigFile"
if (-not (Test-Path $ConfigDir)) {
    New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null
}

$nodeYaml = @"
# Minimal zzrouter node.yaml written by install.ps1
# Defaults to loopback for single-machine use. To expose this node on the LAN
# for multi-node setups, re-run install.ps1 with -Bind 0.0.0.0 (and run
# elevated so the installer can add the firewall rule).
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
  # true: /v1/* and /api/* refuse a request without a key. Written false
  # only for a loopback bind; give LAN clients virtual keys instead.
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
    $existingRole = $null
    $inCluster = $false
    foreach ($line in (Get-Content $ConfigFile)) {
        if ($line -match '^\s*cluster:\s*$') { $inCluster = $true; continue }
        if ($inCluster -and $line -match '^\S') { break }
        if ($inCluster -and $line -match '^\s+mode:\s*([a-z]+)') { $existingRole = $Matches[1]; break }
    }
    if ($existingRole) {
        # Follow the file, so the checks below verify what is really
        # running rather than what the command line asked for.
        $Role = $existingRole
    }
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

# --- Write .env with generated keys (parity with install.sh) ------------
# Mirrors install.sh: auth keys are referenced by name in node.yaml/cli.yaml
# and resolved at runtime by the dotenv loader reading this .env file.
# Production auth stays on; single-machine use works without any env-var
# plumbing because both client and server read from this file.
$EnvFile = Join-Path $ConfigDir ".env"
Write-Step "Writing $EnvFile"

function New-ZzRandomHex {
    param([int]$Bytes = 24)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $buf = New-Object byte[] $Bytes
    $rng.GetBytes($buf)
    -join ($buf | ForEach-Object { $_.ToString("x2") })
}

if (-not (Test-Path $EnvFile)) {
    $AdminKey   = "zzr_" + (New-ZzRandomHex 24)
    $UserKey    = "zzr_" + (New-ZzRandomHex 24)

    # Note: `$` in PowerShell's (?m) multiline regex anchors before \n only,
    # so we tolerate an optional \r via \s* to match CRLF-terminated lines.
    # And we write the final file without a BOM via [System.IO.File]::WriteAllText
    # -- Set-Content -Encoding UTF8 on Windows PowerShell 5.1 emits a BOM, which
    # some dotenv parsers don't strip and which would break the very first line.
    # Cluster traffic uses mTLS on the cluster-port listener (pkg/clusternode),
    # so no shared cluster secret is seeded.
    $EnvTemplate = Join-Path (Split-Path -Parent $BundleDir) "pkg\config\templates\files\env-default"
    if (Test-Path $EnvTemplate) {
        $content = (Get-Content $EnvTemplate -Raw) `
            -replace '(?m)^# ZZROUTER_ADMIN_API_KEY=\s*$', "ZZROUTER_ADMIN_API_KEY=$AdminKey" `
            -replace '(?m)^# ZZROUTER_API_KEY=\s*$',       "# ZZROUTER_API_KEY=$UserKey"
    } else {
        $content = @(
            "ZZROUTER_ADMIN_API_KEY=$AdminKey",
            "# ZZROUTER_API_KEY=$UserKey"
        ) -join "`r`n"
    }
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($EnvFile, $content, $utf8NoBom)

    # Tighten ACL to the current user only (Windows equivalent of chmod 600).
    # Failure here is non-fatal -- the installer still proceeds.
    try {
        $acl = Get-Acl $EnvFile
        $acl.SetAccessRuleProtection($true, $false)  # disable inheritance, drop inherited ACEs
        $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
            [System.Security.Principal.WindowsIdentity]::GetCurrent().User,
            "FullControl", "Allow")
        $acl.ResetAccessRule($rule)
        Set-Acl -Path $EnvFile -AclObject $acl
    } catch {
        Write-Warn2 "Could not tighten ACL on .env: $($_.Exception.Message)"
    }
    Write-Ok "Generated admin/user keys in .env"
} else {
    if (-not (Select-String -Path $EnvFile -Pattern '^ZZROUTER_ADMIN_API_KEY=' -Quiet)) {
        $AdminKey = "zzr_" + (New-ZzRandomHex 24)
        Add-Content -Path $EnvFile -Value ""
        Add-Content -Path $EnvFile -Value "ZZROUTER_ADMIN_API_KEY=$AdminKey"
        Write-Ok "Appended generated ZZROUTER_ADMIN_API_KEY to existing .env"
    }
}

# --- Firewall rules (only needed for non-loopback binds, admin only) ----
# Both ports, not just the admin one. A cluster node that only opens
# $Port pairs successfully and then sits at health "unknown" forever,
# because every coordinator-to-worker call goes to the mTLS listener on
# $ClusterPort instead.
$FirewallPorts = @($Port)
if ($Role -ne "disabled") { $FirewallPorts += $ClusterPort }

if ($IsLoopback) {
    Write-Step "Firewall rules: not needed (bind=$Bind is loopback)"
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
    & $nodeExe start
    if ($LASTEXITCODE -ne 0) {
        Write-Err "zzrouter-node start (service install) exited with code $LASTEXITCODE"
        exit 2
    }
    Write-Ok "Windows service installed and started"
} else {
    & $nodeExe start --no-setup --detach
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
try {
    $detail = (Invoke-WebRequest -Uri "http://127.0.0.1:$Port/health?detailed=true" -UseBasicParsing -TimeoutSec 5).Content | ConvertFrom-Json
    $ident = $detail.details.node_identity
} catch {
    $ident = $null
}

if (-not $ident) {
    Write-Warn2 "Could not read node_identity from /health?detailed=true."
    Write-Warn2 "Cannot confirm which node.yaml the running server loaded."
} else {
    $loaded = $ident.config_path
    $mode   = $ident.cluster_mode
    if ($loaded -and ($loaded -ne $ConfigFile)) {
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
    Write-Ok "Running node loaded $loaded (cluster mode: $mode)"
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
    Write-Host "This node is bound to loopback only (single-machine use)." -ForegroundColor Cyan
    Write-Host "To join other nodes on the LAN later, re-run elevated:"
    Write-Host "  powershell -ExecutionPolicy Bypass -File install.ps1 -Bind 0.0.0.0 -Role worker"
} elseif ($Role -eq "worker") {
    Write-Host "Next step: pair this worker with your coordinator." -ForegroundColor Cyan
    Write-Host "  Here:            zzrouter-node cluster pair --coordinator-url https://COORDINATOR_HOST:$ClusterPort"
    Write-Host "  On the coord:    zzrouter-node cluster accept CODE"
    Write-Host ""
    # $Bind is what this node listens on; 0.0.0.0 is not an address the
    # coordinator can dial. Report a real LAN address instead, because
    # this line exists to be pasted into the coordinator's config.
    Write-Host "The coordinator reaches this node at $(Get-ReachableAddress):$ClusterPort, so"
    Write-Host "that port has to stay open and its cluster.bind_port has to match."
} elseif ($Role -eq "coordinator") {
    Write-Host "This node is a coordinator. To add a worker:" -ForegroundColor Cyan
    Write-Host "  On the worker:   zzrouter-node cluster pair --coordinator-url https://THIS_HOST:$ClusterPort"
    Write-Host "  Here:            zzrouter-node cluster accept CODE"
} else {
    Write-Host "This node is not clustered (cluster.mode: disabled)." -ForegroundColor Cyan
    Write-Host "To make it a worker, re-run with -Role worker, then pair:"
    Write-Host "  zzrouter-node cluster pair --coordinator-url https://COORDINATOR_HOST:$ClusterPort"
}
