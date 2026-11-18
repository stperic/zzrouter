# Source-level fixture: no installer, shortcut, service or browser is started.
$ErrorActionPreference = "Stop"
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot "../../.."))
$source = Join-Path $repo "scripts/install.ps1"
$parseTokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($source, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count -ne 0) { throw "Installer source does not parse." }
$function = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Get-ConsoleShortcutSpec" }, $true)
if (-not $function) { throw "Missing shared console shortcut builder." }
. ([scriptblock]::Create($function.Extent.Text))

$base = [IO.Path]::GetTempPath()
$installDir = Join-Path $base "Router O'Brien `$literal ``tick&"
$configDir = Join-Path $base "Config O'Brien `$literal ``tick&"
$systemDir = Join-Path $base "Windows System"
$spec = Get-ConsoleShortcutSpec -InstallDir $installDir -ConfigDir $configDir -SystemDirectory $systemDir
if ($spec.TargetPath -ne (Join-Path $systemDir "WindowsPowerShell\v1.0\powershell.exe")) { throw "Shortcut must launch Windows PowerShell." }
if ($spec.WorkingDirectory -ne $configDir) { throw "Shortcut lost the selected configuration directory." }
if ($spec.IconLocation -ne ((Join-Path $installDir "zzrouter.exe") + ",0")) { throw "Shortcut lost the installed client icon." }
if ($spec.Arguments -notmatch '^\-NoProfile \-NoExit \-Command "(.*)"$') { throw "Console window must remain open without loading a profile." }

$command = $Matches[1]
$commandAst = [System.Management.Automation.Language.Parser]::ParseInput($command, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count -ne 0) { throw "Shortcut command does not parse." }
$statements = $commandAst.EndBlock.Statements
if ($statements.Count -ne 2) { throw "Paths introduced extra commands." }
$assignment = $statements[0]
if ($assignment.Left.VariablePath.UserPath -ne "env:ZZROUTER_CONFIG_DIR") { throw "Shortcut must explicitly select its installation configuration." }
$configLiteral = $assignment.Right.Expression
if ($configLiteral -isnot [System.Management.Automation.Language.StringConstantExpressionAst] -or $configLiteral.Value -ne $configDir) { throw "Configuration path is not a preserved literal." }
$invocation = $statements[1].PipelineElements[0]
if ($invocation.InvocationOperator -ne [System.Management.Automation.Language.TokenKind]::Ampersand) { throw "Client must be invoked directly." }
if ($invocation.CommandElements.Count -ne 2) { throw "Unexpected console arguments." }
$clientLiteral = $invocation.CommandElements[0]
if ($clientLiteral -isnot [System.Management.Automation.Language.StringConstantExpressionAst] -or $clientLiteral.Value -ne (Join-Path $installDir "zzrouter.exe")) { throw "Client path is not a preserved literal." }
if ($invocation.CommandElements[1].Value -ne "console") { throw "Shortcut does not open the console." }

$native = [IO.File]::ReadAllText((Join-Path $repo "scripts/packaging/windows.iss"))
if ($native -notmatch '-InstallDir[^\r\n]+-NoOpenConsole') { throw "Native install must defer console opening until setup finishes." }
if ($native -notmatch '(?m)^Filename: "\{userprograms\}\\zzRouter\\Router console\.lnk";[^\r\n]+Flags: [^\r\n]*shellexec[^\r\n]*postinstall[^\r\n]*nowait[^\r\n]*skipifsilent') { throw "Native setup must offer the same persistent console shortcut." }
Write-Output "Windows console shortcut fixture passed."
