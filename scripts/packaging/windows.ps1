[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BundleDir,
    [Parameter(Mandatory)][string]$OutputDir,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Za-z.+-]+$')][string]$Version,
    [string]$Compiler = "ISCC.exe",
    [string]$SignToolCommand,
    [switch]$Unsigned
)
$ErrorActionPreference = "Stop"
if (-not $Unsigned -and -not $SignToolCommand) { throw "Supply the Inno Setup signing command, or explicitly use -Unsigned for local qualification." }
$BundleDir = (Resolve-Path $BundleDir).Path
foreach ($file in @("zzrouter.exe", "zzrouter-node.exe", "zzrouter-launcher.exe", "install.ps1", "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES")) {
    if (-not (Test-Path (Join-Path $BundleDir $file) -PathType Leaf)) { throw "Incomplete bundle: $file" }
}
if (-not $Unsigned) {
    # The launcher must already have been signed before its embedded node hash.
    foreach ($binary in @("zzrouter.exe", "zzrouter-node.exe", "zzrouter-launcher.exe")) {
        if ((Get-AuthenticodeSignature (Join-Path $BundleDir $binary)).Status -ne "Valid") { throw "Unsigned or invalid binary: $binary. Sign during the release build before packaging." }
    }
}
if (Test-Path $OutputDir) { throw "Choose a new output directory." }
New-Item -ItemType Directory -Path $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path
$compilerArgs = @("/DBundleDir=$BundleDir", "/DReleaseVersion=$Version", "/DPackageOutput=$OutputDir")
if ($Unsigned) { $compilerArgs += "/DUnsigned=1" } else { $compilerArgs += "/Szzrouter=$SignToolCommand" }
$compilerArgs += (Join-Path $PSScriptRoot "windows.iss")
& $Compiler @compilerArgs
if ($LASTEXITCODE -ne 0) { throw "Windows installer packaging failed." }
