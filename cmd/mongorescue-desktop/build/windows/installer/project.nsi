Unicode true

####
## MongoRescue installer, based on the Wails v2.16.0 template (pkg/buildassets/build/windows/installer/project.nsi).
## "wails build -nsis" keeps this file and regenerates "wails_tools.nsh" next to it on every build, with the
## values from wails.json; the bootstrapper lands in "tmp\MicrosoftEdgeWebview2Setup.exe".
##
## Changes from the template:
##  - WebView2: the template's wails.webview2runtime macro checks only the per-machine key (and the per-user key
##    only for per-user installers) and then runs the ~2 MB online bootstrapper with /silent, which downloads
##    and installs the runtime (well over 100 MB) with no visible progress: on Windows Server, which ships
##    without WebView2, the installer looked hung. mongorescue.webview2runtime below checks the per-machine
##    (64- and 32-bit views) and per-user keys, ignores the "0.0.0.0" version left by a removed runtime, says
##    what it is doing, runs the bootstrapper with its own progress window (silent only for /S installs) and
##    reports the result instead of ignoring it.
##  - Icons: MUI_ICON/MUI_UNICON use build\windows\icon.ico, the MongoRescue logo.
##  - MongoDB Database Tools: the files in build\bin\tools (mongodump.exe, mongorestore.exe and MongoDB's license
##    files, staged by the release workflow) are installed to $INSTDIR\tools, where MongoRescue finds them before PATH,
##    replacing the tools of an earlier install. Without build\bin\tools\mongodump.exe (a local build) the
##    installer is built without them and makensis prints a warning.
##  - Per-user install by default: WAILS_INSTALL_SCOPE defaults to "user" and REQUEST_EXECUTION_LEVEL to "user", so
##    the installer runs without a UAC prompt, installs to $LOCALAPPDATA\Programs\${INFO_PRODUCTNAME} and writes the
##    shortcuts (SetShellVarContext current) and the uninstall key under HKCU; the desktop app then updates itself in
##    place. Build with -DWAILS_INSTALL_SCOPE=machine for the former per-machine install in Program Files. The WebView2
##    bootstrapper may still ask for administrator rights.
##  - Updates of a copy the user cannot write to (a per-machine install in Program Files): the desktop app starts the
##    installer with "/S /RELAUNCH=<DOMAIN\user> /WAITPID=<pid>" and quits. Before the files are copied, the installer
##    waits up to 180 s for that process to exit, then up to 180 s for a running ${PRODUCT_EXECUTABLE} in $INSTDIR,
##    which outlasts the app's shutdown budget; after a silent install it starts the new version only when it runs
##    as the user named by /RELAUNCH (compared case-insensitively), so an administrator who answered the UAC prompt
##    with their own credentials does not get the app started under their account. Interactive installs are unchanged.
##
## For development first make a wails nsis build to populate "wails_tools.nsh":
## > wails build -platform windows/amd64 -nsis
## Then call makensis on this file with the path to the binary:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\MongoRescue.exe project.nsi
####

# Per-user install unless the build asks for another scope (see above). wails_tools.nsh only defines
# REQUEST_EXECUTION_LEVEL ("admin") when it is not defined yet.
!ifndef WAILS_INSTALL_SCOPE
    !define WAILS_INSTALL_SCOPE "user"
!endif
!if "${WAILS_INSTALL_SCOPE}" == "user"
    !ifndef REQUEST_EXECUTION_LEVEL
        !define REQUEST_EXECUTION_LEVEL "user"
    !endif
!endif

!include "wails_tools.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"

# The version information for this two must consist of 4 parts
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe"
!ifdef WAILS_INSTALL_SCOPE
  !if "${WAILS_INSTALL_SCOPE}" == "user"
    InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
  !else
    InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
  !endif
!else
  InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
!endif
ShowInstDetails show

!define WEBVIEW2_CLIENT_KEY "Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"
!define WEBVIEW2_DOWNLOAD_URL "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

# Sets $0 to the installed WebView2 Runtime version, or to "" when none is installed. Per-machine installs
# register under WOW6432Node on 64-bit Windows, per-user installs under HKCU; "0.0.0.0" marks a removed runtime.
Function WebView2Version
    SetRegView 64
    ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\${WEBVIEW2_CLIENT_KEY}" "pv"
    ${If} $0 == ""
    ${OrIf} $0 == "0.0.0.0"
        ReadRegStr $0 HKLM "SOFTWARE\${WEBVIEW2_CLIENT_KEY}" "pv"
    ${EndIf}
    ${If} $0 == ""
    ${OrIf} $0 == "0.0.0.0"
        ReadRegStr $0 HKCU "Software\${WEBVIEW2_CLIENT_KEY}" "pv"
    ${EndIf}
    ${If} $0 == "0.0.0.0"
        StrCpy $0 ""
    ${EndIf}
    ClearErrors
FunctionEnd

# Installs the WebView2 Runtime with Microsoft's online bootstrapper when it is missing. A failure does not
# abort the setup: the app itself offers the download when it starts without the runtime.
!macro mongorescue.webview2runtime
    Call WebView2Version
    ${If} $0 != ""
        DetailPrint "Microsoft Edge WebView2 Runtime $0 is already installed."
    ${Else}
        DetailPrint "Microsoft Edge WebView2 Runtime not found."
        DetailPrint "Installing Microsoft Edge WebView2 Runtime (downloaded from Microsoft, about 150 MB; this can take several minutes)..."
        # Keep the status line on the message above while the bootstrapper runs.
        SetDetailsPrint listonly
        InitPluginsDir
        CreateDirectory "$PLUGINSDIR\webview2bootstrapper"
        SetOutPath "$PLUGINSDIR\webview2bootstrapper"
        File "tmp\MicrosoftEdgeWebview2Setup.exe"
        StrCpy $1 "not started"
        ${If} ${Silent}
            ExecWait '"$PLUGINSDIR\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install' $1
        ${Else}
            ExecWait '"$PLUGINSDIR\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /install' $1
        ${EndIf}
        SetDetailsPrint both
        Call WebView2Version
        ${If} $0 != ""
            DetailPrint "Microsoft Edge WebView2 Runtime $0 installed."
        ${Else}
            DetailPrint "The WebView2 Runtime was not installed (bootstrapper exit code: $1)."
            MessageBox MB_OK|MB_ICONEXCLAMATION "The Microsoft Edge WebView2 Runtime could not be installed (exit code: $1).$\r$\n$\r$\nMongoRescue needs it to open its window. Install it from ${WEBVIEW2_DOWNLOAD_URL} and start MongoRescue again." /SD IDOK
        ${EndIf}
    ${EndIf}
!macroend

# Installs the bundled MongoDB Database Tools to $INSTDIR\tools, removing the tools of an earlier install first.
# Paths are relative to this script (build\windows\installer).
!macro mongorescue.tools
    !if /FileExists "..\..\bin\tools\mongodump.exe"
        DetailPrint "Installing the MongoDB Database Tools (mongodump, mongorestore)..."
        RMDir /r "$INSTDIR\tools"
        SetOutPath "$INSTDIR\tools"
        File /r "..\..\bin\tools\*"
        SetOutPath $INSTDIR
    !else
        !warning "..\..\bin\tools\mongodump.exe not found: building the installer without the MongoDB Database Tools"
    !endif
!macroend

# Waits for the desktop app to exit, so it can be replaced and does not hold the data directory when the new version
# starts: the app quits right after it starts the installer for an update. With /WAITPID=<pid> it first waits up to
# 180 s for that process (a PID that no longer exists returns at once). Then it waits up to 180 s for a running
# ${PRODUCT_EXECUTABLE} in $INSTDIR: a running executable cannot be opened for writing, so the file is opened for
# append (which leaves it unchanged) every 500 ms. Both waits outlast the app's shutdown (30 s for the runs plus the
# drains). When it still runs, an interactive install asks to close it and retry; a silent install aborts, and the
# installed version stays as it was.
Function WaitForApp
    ${GetParameters} $R0
    ClearErrors
    ${GetOptions} $R0 "/WAITPID=" $R2
    ${IfNot} ${Errors}
    ${AndIf} $R2 != ""
        # SYNCHRONIZE (0x00100000): enough to wait for the process.
        System::Call 'kernel32::OpenProcess(i 0x00100000, i 0, i $R2) p .R3'
        ${If} $R3 <> 0
            DetailPrint "Waiting for ${INFO_PRODUCTNAME} to close..."
            System::Call 'kernel32::WaitForSingleObject(p R3, i 180000) i .R4'
            System::Call 'kernel32::CloseHandle(p R3)'
        ${EndIf}
    ${EndIf}
    ClearErrors
    ${IfNot} ${FileExists} "$INSTDIR\${PRODUCT_EXECUTABLE}"
        Return
    ${EndIf}
    StrCpy $R1 0
    ${Do}
        ClearErrors
        FileOpen $R0 "$INSTDIR\${PRODUCT_EXECUTABLE}" a
        ${IfNot} ${Errors}
            FileClose $R0
            Return
        ${EndIf}
        ${If} $R1 == 0
            DetailPrint "Waiting for ${INFO_PRODUCTNAME} to close..."
        ${EndIf}
        IntOp $R1 $R1 + 1
        ${If} $R1 >= 360
            MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${INFO_PRODUCTNAME} is still running.$\r$\n$\r$\nClose it and click Retry to continue the installation." /SD IDCANCEL IDRETRY wait_retry
            Abort "${INFO_PRODUCTNAME} is still running."
            wait_retry:
            StrCpy $R1 1
        ${EndIf}
        Sleep 500
    ${Loop}
FunctionEnd

# Sets $R2 to the account the installer runs as, DOMAIN\user (GetUserNameExW with NameSamCompatible = 2, the form
# the desktop app passes), falling back to %USERDOMAIN%\%USERNAME%.
Function InstallerUser
    StrCpy $R2 ""
    System::Call 'secur32::GetUserNameExW(i 2, w .R2, *i ${NSIS_MAX_STRLEN}) i .R3'
    ${If} $R3 == 0
    ${OrIf} $R2 == ""
        ReadEnvStr $R2 "USERDOMAIN"
        ReadEnvStr $R3 "USERNAME"
        StrCpy $R2 "$R2\$R3"
    ${EndIf}
FunctionEnd

# Starts the installed app after a silent install with /RELAUNCH=<DOMAIN\user> (the update started by the desktop
# app), but only when the installer runs as that user (LogicLib's == ignores case): when another administrator
# answered the UAC prompt of a per-machine installer, the installer runs as them, and the app must not. explorer.exe
# hands the start to the signed-in user's shell, so the app runs without administrator rights, as when it is started
# from the Start menu.
Function RelaunchApp
    ${IfNot} ${Silent}
        Return
    ${EndIf}
    ${GetParameters} $R0
    ClearErrors
    ${GetOptions} $R0 "/RELAUNCH=" $R1
    ${If} ${Errors}
    ${OrIf} $R1 == ""
        ClearErrors
        Return
    ${EndIf}
    Call InstallerUser
    ${If} $R1 != $R2
        DetailPrint "Not starting ${INFO_PRODUCTNAME}: the installer runs as $R2, the update was started by $R1."
        Return
    ${EndIf}
    DetailPrint "Starting ${INFO_PRODUCTNAME}..."
    Exec '"$WINDIR\explorer.exe" "$INSTDIR\${PRODUCT_EXECUTABLE}"'
FunctionEnd

Function .onInit
   !insertmacro wails.checkArchitecture
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro mongorescue.webview2runtime

    DetailPrint "Installing ${INFO_PRODUCTNAME} ${INFO_PRODUCTVERSION}..."
    SetOutPath $INSTDIR

    Call WaitForApp

    !insertmacro wails.files

    !insertmacro mongorescue.tools

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols

    !insertmacro wails.writeUninstaller

    # Tell the shell that icons may have changed (SHCNE_ASSOCCHANGED), so shortcuts and
    # taskbar pins of older installs drop their cached icon (such as the default Wails "W").
    System::Call 'shell32::SHChangeNotify(i 0x08000000, i 0, p 0, p 0)'

    Call RelaunchApp
SectionEnd

Section "uninstall"
    !insertmacro wails.setShellContext

    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    RMDir /r "$INSTDIR\tools" # The bundled MongoDB Database Tools
    RMDir /r $INSTDIR

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    !insertmacro wails.deleteUninstaller

    # Refresh the shell's icon cache (SHCNE_ASSOCCHANGED) for the removed shortcuts.
    System::Call 'shell32::SHChangeNotify(i 0x08000000, i 0, p 0, p 0)'
SectionEnd
