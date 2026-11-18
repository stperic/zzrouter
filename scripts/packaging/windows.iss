#ifndef BundleDir
  #error BundleDir is required
#endif
#ifndef ReleaseVersion
  #error ReleaseVersion is required
#endif
#ifndef PackageOutput
  #error PackageOutput is required
#endif

[Setup]
AppId={{8BD551CD-2304-4CA3-883A-703F82AF01E9}
AppName=zzRouter
AppVersion={#ReleaseVersion}
DefaultDirName={autopf}\zzrouter
DefaultGroupName=zzRouter
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#PackageOutput}
OutputBaseFilename=zzrouter-windows-amd64-{#ReleaseVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayName=zzRouter
UninstallDisplayIcon={app}\zzrouter.exe
#ifndef Unsigned
SignTool=zzrouter
SignedUninstaller=yes
#else
OutputBaseFilename=zzrouter-windows-amd64-{#ReleaseVersion}-unsigned
#endif

[Files]
Source: "{#BundleDir}\*"; DestDir: "{tmp}\zzrouter-release"; Flags: ignoreversion deleteafterinstall

[Run]
Filename: "{userprograms}\zzRouter\Router console.lnk"; Description: "Open Router console"; Flags: shellexec postinstall nowait skipifsilent runascurrentuser; Check: OpensConsole

[UninstallRun]
Filename: "{sys}\sc.exe"; Parameters: "stop zzrouter"; Flags: waituntilterminated; Check: IsAdminInstallMode
Filename: "{sys}\sc.exe"; Parameters: "delete zzrouter"; Flags: waituntilterminated; Check: IsAdminInstallMode
Filename: "{app}\zzrouter-node.exe"; Parameters: "stop"; Flags: waituntilterminated skipifdoesntexist

[UninstallDelete]
Type: files; Name: "{app}\zzrouter.exe"
Type: files; Name: "{app}\zzrouter-node.exe"
Type: files; Name: "{app}\zzrouter-launcher.exe"
Type: dirifempty; Name: "{app}"
Type: files; Name: "{userprograms}\zzRouter\Router console.lnk"
Type: dirifempty; Name: "{userprograms}\zzRouter"

[Code]
var
  RolePage: TInputOptionWizardPage;
  ConnectionPage: TInputQueryWizardPage;
  EffectiveRole: String;

procedure InitializeWizard;
begin
  RolePage := CreateInputOptionPage(wpSelectDir, 'Set up this computer',
    'Create a router or join one you already have.',
    'A coordinator can also run models. Existing installations keep their configured role. Choose an installation for all users to start at boot; a personal installation runs for this session.', True, False);
  RolePage.Add('Create a router');
  RolePage.Add('Join an existing router as a worker');
  RolePage.SelectedValueIndex := 0;
  ConnectionPage := CreateInputQueryPage(RolePage.ID, 'Connect to your router',
    'Match your coordinator’s cluster port.',
    'Open Nodes > Add compute node in your coordinator console. Enter the Worker TCP port shown under Connection details.');
  ConnectionPage.Add('Worker TCP port:', False);
  ConnectionPage.Values[0] := '9091';
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := (PageID = ConnectionPage.ID) and (RolePage.SelectedValueIndex <> 1);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var Port: Integer;
begin
  Result := True;
  if CurPageID = ConnectionPage.ID then begin
    Port := StrToIntDef(ConnectionPage.Values[0], 0);
    Result := (Port >= 1) and (Port <= 65535);
    if not Result then MsgBox('Enter a port from 1 to 65535.', mbError, MB_OK);
  end;
end;

function OpensConsole: Boolean;
begin
  Result := EffectiveRole = 'coordinator';
end;

function PowerShellQuote(Value: String): String;
begin
  StringChangeEx(Value, '''', '''''', True);
  Result := '''' + Value + '''';
end;

function InstalledRole: String;
var
  ConfigDir, Command: String;
  ExitCode: Integer;
  Output: TExecOutput;
begin
  ConfigDir := GetEnv('ZZROUTER_CONFIG_DIR');
  if ConfigDir = '' then begin
    if IsAdminInstallMode then ConfigDir := ExpandConstant('{commonappdata}\zzrouter')
    else ConfigDir := ExpandConstant('{userappdata}\zzrouter');
  end;
  Command := '-NoProfile -Command "$identity = & ' + PowerShellQuote(ExpandConstant('{app}\zzrouter-node.exe')) +
    ' config identity --dir ' + PowerShellQuote(ConfigDir) +
    ' --json | ConvertFrom-Json; if ($LASTEXITCODE -ne 0) { exit 1 }; $identity.mode"';
  if not ExecAndCaptureOutput(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'), Command, '', SW_SHOWNORMAL, ewWaitUntilTerminated, ExitCode, Output) then
    RaiseException('Could not read the installed router role. Configuration was preserved.');
  if (ExitCode <> 0) or Output.Error or (GetArrayLength(Output.StdOut) <> 1) then
    RaiseException('Could not confirm the installed router role. Configuration was preserved.');
  Result := Trim(Output.StdOut[0]);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Arguments: String;
  ExitCode: Integer;
begin
  if CurStep = ssPostInstall then begin
    Arguments := '-NoProfile -ExecutionPolicy Bypass -File "' + ExpandConstant('{tmp}\zzrouter-release\install.ps1') + '" -InstallDir "' + ExpandConstant('{app}') + '" -NoOpenConsole';
    if IsAdminInstallMode then Arguments := Arguments + ' -Service'
    else Arguments := Arguments + ' -PerUser';
    if RolePage.SelectedValueIndex = 1 then Arguments := Arguments + ' -Role worker -ClusterPort ' + IntToStr(StrToIntDef(ConnectionPage.Values[0], 9091));
    if not Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'), Arguments, '', SW_SHOWNORMAL, ewWaitUntilTerminated, ExitCode) then
      RaiseException('Could not launch router installation. Existing configuration was preserved.');
    if ExitCode <> 0 then
      RaiseException('Router installation did not complete. Review the installer output before retrying.');
    EffectiveRole := InstalledRole;
    if EffectiveRole = 'worker' then
      WizardForm.FinishedLabel.Caption := 'Worker installed. Open Nodes > Add compute node in your coordinator console. Select Windows and the same installation type (all users or this account). Open a new PowerShell window, then run the connection command. Skip installation because this worker is already installed.';
  end;
end;
