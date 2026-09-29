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
##
## For development first make a wails nsis build to populate "wails_tools.nsh":
## > wails build -platform windows/amd64 -nsis
## Then call makensis on this file with the path to the binary:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\MongoRescue.exe project.nsi
####
!include "wails_tools.nsh"
!include "LogicLib.nsh"

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

Function .onInit
   !insertmacro wails.checkArchitecture
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro mongorescue.webview2runtime

    DetailPrint "Installing ${INFO_PRODUCTNAME} ${INFO_PRODUCTVERSION}..."
    SetOutPath $INSTDIR

    !insertmacro wails.files

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols

    !insertmacro wails.writeUninstaller

    # Tell the shell that icons may have changed (SHCNE_ASSOCCHANGED), so shortcuts and
    # taskbar pins of older installs drop their cached icon (such as the default Wails "W").
    System::Call 'shell32::SHChangeNotify(i 0x08000000, i 0, p 0, p 0)'
SectionEnd

Section "uninstall"
    !insertmacro wails.setShellContext

    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    RMDir /r $INSTDIR

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    !insertmacro wails.deleteUninstaller

    # Refresh the shell's icon cache (SHCNE_ASSOCCHANGED) for the removed shortcuts.
    System::Call 'shell32::SHChangeNotify(i 0x08000000, i 0, p 0, p 0)'
SectionEnd
