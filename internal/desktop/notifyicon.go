package desktop

// notifyIconData is the Win32 NOTIFYICONDATAW structure (Windows Vista and
// later), which Tray.Notify passes to Shell_NotifyIconW. It holds no Windows
// types, so its layout is tested on every platform: 976 bytes on 64-bit
// Windows, with szInfoTitle at offset 820 and dwInfoFlags at 948.
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr // HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr // HICON
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	// TimeoutOrVersion is the union of uTimeout and uVersion.
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUIDItem         [16]byte // GUID
	BalloonIcon      uintptr  // HICON
}
