; all2api — Windows installer (Inno Setup 6).
;
; Built by scripts/package-windows-setup.ps1, which supplies the defines:
;   MyVersion       version string shown in the wizard / Add-Remove Programs
;   StageDir        folder holding all2api.exe + config.example.json
;   OutputDir       where the setup exe lands
;   OutputBaseName  setup exe file name (no extension)
;   SeedDir         optional; credential files copied into auths\ on first
;                   install only (onlyifdoesntexist — newer tokens survive
;                   upgrades). Empty/undefined = standard package.
;
; User data (config.json / auths\ / data\) is never overwritten or removed by
; upgrade; uninstall keeps it too (seeded auth files carry
; uninsneveruninstall, and config.json is created post-install so it is not
; tracked). A clean machine removal is "uninstall, then delete the folder".

#ifndef MyVersion
  #define MyVersion "dev"
#endif
#ifndef StageDir
  #define StageDir "..\..\dist\inst-stage"
#endif
#ifndef OutputDir
  #define OutputDir "..\..\dist"
#endif
#ifndef OutputBaseName
  #define OutputBaseName "all2api-setup"
#endif
#ifdef SeedDir
  #define SeedEnabled
#endif

[Setup]
AppId={{7A3F9C2E-4B1D-4E8A-9F60-A2D4C8E1B5F3}
AppName=all2api
AppVersion={#MyVersion}
AppVerName=all2api {#MyVersion}
AppPublisher=all2api
DefaultDirName={autopf}\all2api
DefaultGroupName=all2api
DisableProgramGroupPage=yes
; Full wizard with an install-directory page; admin or per-user is chosen via
; the standard privilege dialog when the setup is run unelevated.
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog commandline
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
CloseApplications=yes
UninstallDisplayIcon={app}\all2api.exe
OutputDir={#OutputDir}
OutputBaseFilename={#OutputBaseName}
ShowLanguageDialog=no

[Messages]
SetupWindowTitle=安装 — [name]
WelcomeLabel1=欢迎安装 [name]
WelcomeLabel2=接下来会把 [name/ver] 安装到您的电脑。%n%n继续前建议先退出正在运行的 all2api。
SelectDirLabel3=安装程序将把 [name] 安装到以下文件夹（可直接修改或浏览选择）。
SelectDirBrowseLabel=点击“下一步”继续；要更换文件夹，请点击“浏览”。
SelectTasksLabel2=请选择安装时要执行的其他任务，然后点击“下一步”。
ReadyLabel1=安装程序已准备好开始安装 [name]。
ReadyLabel2a=点击“安装”开始安装；要检查或更改设置，点击“上一步”。
FinishedHeadingLabel=[name] 安装完成
FinishedLabel=安装完成。可通过“开始”菜单或桌面快捷方式启动 all2api。%n%n首次使用：浏览器打开 http://localhost:7800
ConfirmUninstall=确定要卸载 %1 吗？%n%n您的账号凭证（auths\）、配置（config.json）和数据（data\）将保留，不会删除。
PrivilegesRequiredOverrideTitle=选择安装模式
PrivilegesRequiredOverrideInstruction=请选择 all2api 的安装模式。
PrivilegesRequiredOverrideText1=可为这台电脑的所有用户安装（写入 Program Files，需要管理员权限），也可只为您的账号安装（写入您的用户目录，无需管理员）。
PrivilegesRequiredOverrideText2=只能为您的账号安装（写入您的用户目录，无需管理员）。
PrivilegesRequiredOverrideAllUsers=为所有用户安装(&A)
PrivilegesRequiredOverrideAllUsersRecommended=为所有用户安装(&A)（推荐）
PrivilegesRequiredOverrideCurrentUser=仅为我安装(&M)
PrivilegesRequiredOverrideCurrentUserRecommended=仅为我安装(&M)（推荐）
ButtonNext=下一步(&N) >
ButtonBack=< 上一步(&B)
ButtonInstall=安装(&I)
ButtonFinish=完成(&F)
ButtonBrowse=浏览(&B)...
ButtonCancel=取消
ButtonYes=是(&Y)
ButtonNo=否(&N)

[Tasks]
Name: "desktopicon"; Description: "创建桌面快捷方式"; Flags: checkedonce
Name: "autostart"; Description: "登录时自动启动 all2api"; Flags: unchecked
Name: "firewall"; Description: "在 Windows 防火墙放行 TCP 7800 入站（需管理员）"; Flags: unchecked

[Files]
Source: "{#StageDir}\all2api.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#StageDir}\config.example.json"; DestDir: "{app}"; Flags: ignoreversion
#ifdef SeedEnabled
; Machine-specific credential seed: copied only when the target file does not
; exist yet, and never removed by the uninstaller.
Source: "{#SeedDir}\*.json"; DestDir: "{app}\auths"; Flags: onlyifdoesntexist uninsneveruninstall
#endif

[Icons]
Name: "{autoprograms}\all2api"; Filename: "{app}\all2api.exe"; WorkingDir: "{app}"
Name: "{autodesktop}\all2api"; Filename: "{app}\all2api.exe"; WorkingDir: "{app}"; Tasks: desktopicon
Name: "{userstartup}\all2api"; Filename: "{app}\all2api.exe"; WorkingDir: "{app}"; Tasks: autostart

[Run]
Filename: "{app}\all2api.exe"; Description: "立即启动 all2api"; Flags: nowait postinstall skipifsilent
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""all2api"""; Flags: runhidden; Tasks: firewall
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall add rule name=""all2api"" dir=in action=allow protocol=TCP localport=7800"; Flags: runhidden; Tasks: firewall

[UninstallRun]
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""all2api"""; Flags: runhidden; RunOnceId: "DelFirewallRule"

[Code]
procedure CurStepChanged(CurStep: TSetupStep);
begin
  { First install: config.example.json becomes the live config.json. The live
    file is never tracked by the installer, so upgrades and uninstalls leave
    it (and everything the app wrote next to it) untouched. }
  if (CurStep = ssPostInstall) and (not FileExists(ExpandConstant('{app}\config.json'))) then
    CopyFile(ExpandConstant('{app}\config.example.json'), ExpandConstant('{app}\config.json'), False);
end;
