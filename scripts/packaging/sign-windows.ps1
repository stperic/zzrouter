param(
    [Parameter(Mandatory)][string]$Binary,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$Thumbprint,
    [string]$TimestampURL = "http://timestamp.digicert.com"
)
$ErrorActionPreference = "Stop"
& signtool.exe sign /sha1 $Thumbprint /fd SHA256 /tr $TimestampURL /td SHA256 $Binary
if ($LASTEXITCODE -ne 0) { throw "Native binary signing failed." }
& signtool.exe verify /pa $Binary
if ($LASTEXITCODE -ne 0) { throw "Native binary signature verification failed." }
