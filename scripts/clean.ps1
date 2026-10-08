# zzrouter Windows clean script -- mirrors clean.sh for Windows
# Usage:
#   powershell -ExecutionPolicy Bypass -File .\clean.ps1
#   powershell -ExecutionPolicy Bypass -File .\clean.ps1 -RemoveBinaries
#
# By default: stops the running node, removes the Windows service
# "zzrouter" if present, deletes every zzrouter firewall rule, removes a
# managed Ollama from both the operator's and the service account's
# profile, removes every zzrouter config/data root (%APPDATA%,
# %LOCALAPPDATA%, %ProgramData%, and the LocalSystem profile), and clears
# the machine-wide ZZROUTER_CONFIG_DIR. Binaries are preserved (parity
# with clean.sh).
#
# It has to cover all of those because which ones exist depends on how the
# node was installed: install.ps1 without -Service writes the operator's
# profile, with -Service it writes %ProgramData%, and an elevated
# `zzrouter-node start` writes the LocalSystem profile.
#
# -RemoveBinaries also deletes the install directory and removes it from
# PATH (both User and Machine if elevated).

[CmdletBinding()]
param(
    [string]$InstallDir,
    [int]$Port = 9090,
    [switch]$RemoveBinaries
)

$ErrorActionPreference = "Continue"

function Write-Step($msg)  { Write-Host "[*] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)    { Write-Host "[+] $msg" -ForegroundColor Green }
function Write-Warn2($msg) { Write-Host "[!] $msg" -ForegroundColor Yellow }

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
$IsAdmin = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

# The LocalSystem account's profile. A node installed as a service does
# its provider installs from here, not from the operator's profile.
$SystemProfile = Join-Path $env:WINDIR "System32\config\systemprofile"

# --- Stop running node --------------------------------------------------
Write-Step "Stopping zzrouter-node processes"
$procs = Get-Process -Name "zzrouter-node" -ErrorAction SilentlyContinue
if ($procs) {
    $procs | ForEach-Object {
        try {
            $_ | Stop-Process -Force -ErrorAction Stop
            Write-Ok "Stopped PID $($_.Id)"
        } catch {
            Write-Warn2 "Could not stop PID $($_.Id): $($_.Exception.Message)"
        }
    }
    Start-Sleep -Seconds 1
} else {
    Write-Ok "No zzrouter-node processes running"
}

# Also stop any lingering zzrouter-launcher (shouldn't normally run)
Get-Process -Name "zzrouter-launcher" -ErrorAction SilentlyContinue |
    ForEach-Object { $_ | Stop-Process -Force -ErrorAction SilentlyContinue }

# --- Remove Windows Service if installed --------------------------------
# The service is registered as "zzrouter", NOT "zzrouter-node" -- see
# windowsServiceName in pkg/service/windows.go. This script looked for the
# latter and so silently left the real service installed and running,
# which then recreated config directories moments after they were deleted.
$ServiceName = "zzrouter"
Write-Step "Windows Service ($ServiceName)"
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    if ($IsAdmin) {
        try {
            if ($svc.Status -ne "Stopped") {
                Stop-Service -Name $ServiceName -Force -ErrorAction Stop
                # Wait for the process to actually exit: sc.exe delete on a
                # still-running service only marks it for deletion, and the
                # directories below get recreated if it is still alive.
                (Get-Service $ServiceName).WaitForStatus("Stopped", "00:00:30")
            }
            # sc.exe delete -- Remove-Service only exists on PowerShell 6+
            & sc.exe delete $ServiceName | Out-Null
            Write-Ok "Removed service $ServiceName"
        } catch {
            Write-Warn2 "Could not remove service: $($_.Exception.Message)"
        }
    } else {
        Write-Warn2 "Service exists but removal requires Administrator -- skipped"
    }
} else {
    Write-Ok "No $ServiceName service installed"
}

# --- Remove firewall rules ----------------------------------------------
# Matched on prefix rather than one exact name: the installer creates a
# rule per port ("zzrouter 9090" AND "zzrouter 9091"), and hand-made
# variants like "zzrouter cluster 9091" exist on boxes onboarded before
# the installer opened the cluster port. Removing only the admin-port rule
# left the cluster port open on an otherwise uninstalled machine.
Write-Step "Firewall rules matching 'zzrouter*'"
$rules = Get-NetFirewallRule -ErrorAction SilentlyContinue |
    Where-Object { $_.DisplayName -like "zzrouter*" }
if ($rules) {
    if ($IsAdmin) {
        foreach ($r in $rules) {
            Remove-NetFirewallRule -Name $r.Name -ErrorAction SilentlyContinue
            Write-Ok "Removed firewall rule '$($r.DisplayName)'"
        }
    } else {
        Write-Warn2 "Firewall rules exist but removal requires Administrator -- skipped"
    }
} else {
    Write-Ok "No zzrouter firewall rules"
}

# Also remove the exe-based rules Windows auto-creates on first listen
# when the user clicks "Allow" in the Defender prompt.
$autoRules = Get-NetFirewallRule -ErrorAction SilentlyContinue |
    Where-Object { $_.DisplayName -like "*zzrouter-node*" }
if ($autoRules -and $IsAdmin) {
    $autoRules | ForEach-Object {
        Remove-NetFirewallRule -Name $_.Name -ErrorAction SilentlyContinue
        Write-Ok "Removed auto-created rule '$($_.DisplayName)'"
    }
}

# --- Uninstall managed Ollama if present --------------------------------
# Checked per profile, because provider installs follow whichever account
# the node runs as: a detached node installs into the operator's profile,
# a service into LocalSystem's. Looking only at the operator's left a
# multi-GB Ollama and its models behind on every service install.
function Remove-ManagedOllama {
    param([string]$ProfileLocalAppData, [string]$ProfileHome, [string]$Label)

    $marker    = Join-Path $ProfileLocalAppData "zzrouter\providers\ollama\.managed"
    $ollamaDir = Join-Path $ProfileLocalAppData "Programs\Ollama"
    $ollamaExe = Join-Path $ollamaDir "ollama.exe"

    if (-not (Test-Path $marker)) {
        if (Test-Path $ollamaExe) {
            Write-Ok "Ollama in $Label is not managed by zzRouter -- leaving untouched"
        }
        return
    }

    Write-Step "Ollama in $Label was installed by zzRouter -- uninstalling"

    Get-Process ollama,ollama_llama_server -ErrorAction SilentlyContinue |
        ForEach-Object { try { $_ | Stop-Process -Force } catch { } }

    if (Test-Path $ollamaDir) {
        # Inno Setup writes unins000.exe on first install and unins001,
        # unins002... on reinstalls; the highest-numbered one is current.
        $unins = Get-ChildItem $ollamaDir -Filter 'unins*.exe' -ErrorAction SilentlyContinue |
            Sort-Object Name -Descending | Select-Object -First 1
        if ($unins) {
            try {
                Start-Process $unins.FullName -ArgumentList '/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART' -Wait
                Write-Ok "Ollama uninstaller completed: $($unins.Name)"
            } catch {
                Write-Warn2 "Ollama uninstaller failed: $($_.Exception.Message)"
            }
        }

        # Fallback when the uninstaller is missing (corrupted install, AV
        # quarantine) or left files behind: without this the directory
        # lingers with no way to recover through clean.
        if (Test-Path $ollamaExe) {
            Write-Warn2 "Uninstaller missing or incomplete -- removing $ollamaDir directly"
            try {
                Remove-Item $ollamaDir -Recurse -Force -ErrorAction Stop
                Write-Ok "Removed $ollamaDir"
            } catch {
                Write-Warn2 "Could not fully remove ${ollamaDir}: $($_.Exception.Message)"
            }
        }
    }

    # Model data can be many GB.
    $models = Join-Path $ProfileHome ".ollama"
    if (Test-Path $models) {
        $size = (Get-ChildItem $models -Recurse -ErrorAction SilentlyContinue |
            Measure-Object -Property Length -Sum).Sum / 1MB
        Write-Step ("Removing Ollama model data ({0:N0} MB): $models" -f $size)
        try {
            Remove-Item $models -Recurse -Force -ErrorAction Stop
            Write-Ok "Removed $models"
        } catch {
            Write-Warn2 "Could not fully remove ${models}: $($_.Exception.Message)"
        }
    }
}

Remove-ManagedOllama -ProfileLocalAppData $env:LOCALAPPDATA `
                     -ProfileHome $env:USERPROFILE `
                     -Label "this account"
Remove-ManagedOllama -ProfileLocalAppData (Join-Path $SystemProfile "AppData\Local") `
                     -ProfileHome $SystemProfile `
                     -Label "the service account"

# --- Remove every config/data root --------------------------------------
# There is more than one, and which ones exist depends on how the node was
# installed. Removing only the operator's %APPDATA% copy (all this script
# used to do) leaves a service install fully intact: its config lives
# under ProgramData or the LocalSystem profile, so the node keeps its
# identity, its pairing and its providers across a "clean".
#
#   %APPDATA%\zzrouter          operator config (detached install)
#   %LOCALAPPDATA%\zzrouter     operator data: models, providers, logs, pids
#   %ProgramData%\zzrouter      shared config root used by -Service
#   <systemprofile>\...          what a LocalSystem service falls back to
#                                when ZZROUTER_CONFIG_DIR is not set
$ZzRoots = @(
    (Join-Path $env:APPDATA "zzrouter"),
    (Join-Path $env:LOCALAPPDATA "zzrouter"),
    (Join-Path $env:ProgramData "zzrouter"),
    (Join-Path $SystemProfile "AppData\Roaming\zzrouter"),
    (Join-Path $SystemProfile "AppData\Local\zzrouter")
)

foreach ($root in $ZzRoots) {
    if (-not (Test-Path $root)) {
        Write-Ok "Nothing at $root"
        continue
    }
    Write-Step "Removing $root"
    # Provider venvs and extracted archives can carry read-only bits.
    Get-ChildItem -Path $root -Recurse -Force -ErrorAction SilentlyContinue |
        ForEach-Object {
            try { $_.Attributes = 'Normal' } catch { }
        }
    try {
        Remove-Item -Path $root -Recurse -Force -ErrorAction Stop
        Write-Ok "Removed $root"
    } catch {
        Write-Warn2 "Partial removal of ${root}: $($_.Exception.Message)"
    }
}

# --- Clear the machine-wide config pointer ------------------------------
# install.ps1 -Service sets this so the service account and the operator
# CLI resolve one config root. Left behind, it points at a directory that
# no longer exists, and the next install inherits a stale answer.
Write-Step "Machine-wide ZZROUTER_CONFIG_DIR"
if ([Environment]::GetEnvironmentVariable("ZZROUTER_CONFIG_DIR", "Machine")) {
    if ($IsAdmin) {
        [Environment]::SetEnvironmentVariable("ZZROUTER_CONFIG_DIR", $null, "Machine")
        Write-Ok "Cleared machine-wide ZZROUTER_CONFIG_DIR"
    } else {
        Write-Warn2 "ZZROUTER_CONFIG_DIR is set but clearing it requires Administrator -- skipped"
    }
} else {
    Write-Ok "No machine-wide ZZROUTER_CONFIG_DIR"
}

# --- Optionally remove binaries + PATH ----------------------------------
if ($RemoveBinaries) {
    # Default install dirs if not provided
    if (-not $InstallDir) {
        $perUserDir  = Join-Path $env:LOCALAPPDATA "Programs\zzrouter"
        $machineDir  = "C:\Program Files\zzrouter"
        $candidates  = @($perUserDir, $machineDir) | Where-Object { Test-Path $_ }
    } else {
        $candidates = @($InstallDir)
    }

    foreach ($dir in $candidates) {
        Write-Step "Removing install dir: $dir"
        try {
            Remove-Item -Path $dir -Recurse -Force -ErrorAction Stop
            Write-Ok "Removed $dir"
        } catch {
            Write-Warn2 "Could not fully remove ${dir}: $($_.Exception.Message)"
        }

        # Strip from User PATH
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if ($userPath -and $userPath -like "*$dir*") {
            $cleaned = ($userPath -split ';' | Where-Object { $_ -and ($_ -ne $dir) }) -join ';'
            [Environment]::SetEnvironmentVariable("Path", $cleaned, "User")
            Write-Ok "Removed $dir from User PATH"
        }

        # Strip from Machine PATH (admin only)
        if ($IsAdmin) {
            $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
            if ($machinePath -and $machinePath -like "*$dir*") {
                $cleaned = ($machinePath -split ';' | Where-Object { $_ -and ($_ -ne $dir) }) -join ';'
                [Environment]::SetEnvironmentVariable("Path", $cleaned, "Machine")
                Write-Ok "Removed $dir from Machine PATH"
            }
        }
    }

    if (-not $candidates) {
        Write-Ok "No install directory found to remove"
    }
}

Write-Host ""
if ($RemoveBinaries) {
    Write-Ok "Clean complete -- binaries, config, data, and providers removed"
} else {
    Write-Ok "Clean complete -- binaries preserved, all config/data/providers removed"
    Write-Host "    (pass -RemoveBinaries to also delete the install dir and PATH entries)"
}
