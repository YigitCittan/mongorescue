package desktop

import (
	"testing"
	"unsafe"
)

func TestNotifyIconDataLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the layout is checked for 64-bit Windows")
	}
	var nid notifyIconData
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(NOTIFYICONDATAW)", unsafe.Sizeof(nid), 976},
		{"hWnd", unsafe.Offsetof(nid.Wnd), 8},
		{"hIcon", unsafe.Offsetof(nid.Icon), 32},
		{"szTip", unsafe.Offsetof(nid.Tip), 40},
		{"szInfo", unsafe.Offsetof(nid.Info), 304},
		{"uTimeout/uVersion", unsafe.Offsetof(nid.TimeoutOrVersion), 816},
		{"szInfoTitle", unsafe.Offsetof(nid.InfoTitle), 820},
		{"dwInfoFlags", unsafe.Offsetof(nid.InfoFlags), 948},
		{"guidItem", unsafe.Offsetof(nid.GUIDItem), 952},
		{"hBalloonIcon", unsafe.Offsetof(nid.BalloonIcon), 968},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d; want %d", c.name, c.got, c.want)
		}
	}
}
