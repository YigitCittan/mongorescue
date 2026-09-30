package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Win32 entry points that golang.org/x/sys/windows does not wrap.
var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	shell32                      = windows.NewLazySystemDLL("shell32.dll")
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procPostThreadMessageW       = user32.NewProc("PostThreadMessageW")
	procPeekMessageW             = user32.NewProc("PeekMessageW")
	procFindWindowExW            = user32.NewProc("FindWindowExW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
)

// Win32 constants.
const (
	wmQuit      = 0x0012
	wmUser      = 0x0400
	pmNoRemove  = 0x0000
	nimModify   = 0x00000001
	nifInfo     = 0x00000010
	niifInfo    = 0x00000001
	idOK        = 1
	confirmType = windows.MB_OKCANCEL | windows.MB_ICONWARNING | windows.MB_DEFBUTTON2 |
		windows.MB_SETFOREGROUND | windows.MB_TOPMOST | windows.MB_TASKMODAL
)

// fyne.io/systray creates its notification icon with this ID on a window of this
// class; Tray.Notify modifies that icon to show a balloon.
const (
	systrayIconID = 100
	systrayClass  = "SystrayClass"
)

// trayRefresh is how often the tray menu is refreshed without an event.
const trayRefresh = 2 * time.Second

// trayStopTries bounds how often Tray asks the icon's message loop to end, every
// 100 ms.
const trayStopTries = 50

// errTrayNotReady is returned by Tray.Notify before the icon exists.
var errTrayNotReady = errors.New("desktop: the tray icon is not ready")

// UILanguage returns the language of the Windows user interface
// (GetUserDefaultUILanguage): Turkish or English.
func UILanguage() Language {
	id, _, _ := procGetUserDefaultUILanguage.Call()
	return LanguageFromLangID(uint16(id)) //nolint:gosec // G115: a LANGID is 16 bits.
}

// runKeyPath is the per-user Run key.
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// userRunKey is the Run key of the current user.
type userRunKey struct{}

// NewRunKey returns the Run key of the current user,
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
func NewRunKey() RunKey {
	return userRunKey{}
}

// Get implements RunKey.
func (userRunKey) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Set implements RunKey.
func (userRunKey) Set(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(name, value)
}

// Delete implements RunKey.
func (userRunKey) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if err = k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// winMsg is the Win32 MSG structure.
type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	private uint32
}

// ensureMessageQueue makes Windows create the calling thread's message queue, so
// PostThreadMessage to it succeeds.
func ensureMessageQueue() {
	var m winMsg
	_, _, _ = procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, wmUser, wmUser, pmNoRemove) //nolint:gosec // G103: Win32 call.
}

// postQuit posts WM_QUIT to the thread tid, which ends its message loop, or a
// modal loop such as a message box's.
func postQuit(tid uint32) bool {
	r, _, _ := procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	return r != 0
}

// ConfirmBox shows a warning message box with OK and Cancel, Cancel being the
// default, and reports whether the user chose OK. The box closes, as cancelled,
// once ctx ends. It runs on its own locked thread, which ends with it.
func ConfirmBox(ctx context.Context, title, message string) bool {
	if ctx.Err() != nil {
		return false
	}
	t16, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	m16, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return false
	}
	res := make(chan bool, 1)
	go func() {
		// Never unlocked: the thread, with a WM_QUIT left in its queue, ends with
		// the goroutine.
		runtime.LockOSThread()
		ensureMessageQueue()
		tid := windows.GetCurrentThreadId()
		stop := context.AfterFunc(ctx, func() { postQuit(tid) })
		ret, _ := windows.MessageBox(0, m16, t16, confirmType)
		stop()
		res <- ret == idOK && ctx.Err() == nil
	}()
	return <-res
}

// TrayOptions configures NewTray.
type TrayOptions struct {
	// Icon is the icon, in .ico format.
	Icon []byte
	// Background provides the texts, the status and the quit actions.
	Background *Background
	// Autostart backs the "Start with Windows" item; nil leaves the item out.
	Autostart *Autostart
	// Open shows and focuses the window; it must not block.
	Open func()
	// Logger receives the log records; nil means slog.Default().
	Logger *slog.Logger
}

// trayItems are the menu items.
type trayItems struct {
	open, status, shuttingDown, autostart, quit, cancelQuit, forceQuit *systray.MenuItem
}

// Tray is the notification area icon of the app, with its menu, built on
// fyne.io/systray. Its message loop runs on its own locked thread. It is safe for
// concurrent use.
type Tray struct {
	opts  TrayOptions
	texts TrayTexts

	wg       sync.WaitGroup
	started  atomic.Bool
	ready    atomic.Bool
	tid      atomic.Uint32
	runDone  chan struct{} // closed once the message loop ended
	loopDone chan struct{} // closed once the menu loop ended
	items    chan trayItems
	refresh  chan struct{}
	tapped   chan struct{}
}

// NewTray returns a tray for opts; Start shows it.
func NewTray(opts TrayOptions) *Tray {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Tray{
		opts:     opts,
		texts:    opts.Background.Texts(),
		runDone:  make(chan struct{}),
		loopDone: make(chan struct{}),
		items:    make(chan trayItems, 1),
		refresh:  make(chan struct{}, 1),
		tapped:   make(chan struct{}, 1),
	}
}

// Start shows the icon. Its message loop and the menu handling run in goroutines
// bound to ctx: once ctx ends, the icon is removed and they return; Wait waits for
// them. Start may be called once per process.
func (t *Tray) Start(ctx context.Context) {
	if !t.started.CompareAndSwap(false, true) {
		return
	}
	t.wg.Add(2)
	go func() {
		defer t.wg.Done()
		defer close(t.runDone)
		// systray.Run runs the message loop of the icon's window, which must be
		// the thread that created it. Never unlocked: the thread ends with the
		// goroutine.
		runtime.LockOSThread()
		ensureMessageQueue()
		t.tid.Store(windows.GetCurrentThreadId())
		systray.Run(t.onReady, nil)
	}()
	go func() {
		defer t.wg.Done()
		defer close(t.loopDone)
		t.loop(ctx)
	}()
}

// Ready reports whether the icon is shown and its menu handled: its menu was
// built and neither its message loop nor its menu loop has ended.
func (t *Tray) Ready() bool {
	if !t.ready.Load() {
		return false
	}
	select {
	case <-t.runDone:
		return false
	case <-t.loopDone:
		return false
	default:
		return true
	}
}

// Wait waits for the goroutines of Start to return.
func (t *Tray) Wait() {
	t.wg.Wait()
}

// Refresh updates the menu from the current state; it does not block.
func (t *Tray) Refresh() {
	select {
	case t.refresh <- struct{}{}:
	default:
	}
}

// onReady builds the menu. systray calls it on its own goroutine once the icon
// exists.
func (t *Tray) onReady() {
	systray.SetIcon(t.opts.Icon)
	systray.SetTooltip(t.texts.Tooltip)
	systray.SetOnTapped(func() {
		select {
		case t.tapped <- struct{}{}:
		default:
		}
	})
	var it trayItems
	it.open = systray.AddMenuItem(t.texts.Open, "")
	it.status = systray.AddMenuItem(t.opts.Background.Status(), "")
	it.status.Disable()
	it.shuttingDown = systray.AddMenuItem(t.texts.ShuttingDown, "")
	it.shuttingDown.Disable()
	it.shuttingDown.Hide()
	if t.opts.Autostart != nil {
		on, err := t.opts.Autostart.Enabled()
		if err != nil {
			t.opts.Logger.Warn("could not read the autostart entry", slog.Any("error", err))
		}
		it.autostart = systray.AddMenuItemCheckbox(t.texts.Autostart, "", on)
	}
	systray.AddSeparator()
	it.quit = systray.AddMenuItem(t.texts.Quit, "")
	it.cancelQuit = systray.AddMenuItem(t.texts.CancelQuit, "")
	it.cancelQuit.Hide()
	it.forceQuit = systray.AddMenuItem(t.texts.ForceQuit, "")
	t.ready.Store(true)
	select {
	case t.items <- it:
	default:
	}
}

// loop handles the menu until ctx ends, then removes the icon.
func (t *Tray) loop(ctx context.Context) {
	ticker := time.NewTicker(trayRefresh)
	defer ticker.Stop()
	var (
		items   *trayItems
		view    trayView
		open    <-chan struct{}
		auto    <-chan struct{}
		quit    <-chan struct{}
		cancelQ <-chan struct{}
		force   <-chan struct{}
	)
	for {
		select {
		case <-ctx.Done():
			t.stop()
			return
		case <-t.runDone:
			// The window cannot be opened from the tray any more: show it.
			t.opts.Logger.Warn("the tray icon's message loop ended; showing the window")
			t.opts.Open()
			return
		case it := <-t.items:
			items = &it
			open, quit, cancelQ, force = it.open.ClickedCh, it.quit.ClickedCh, it.cancelQuit.ClickedCh, it.forceQuit.ClickedCh
			if it.autostart != nil {
				auto = it.autostart.ClickedCh
			}
			view = trayView{quitShown: true}
		case <-ticker.C:
		case <-t.refresh:
		case <-t.tapped:
			t.opts.Open()
		case <-open:
			t.opts.Open()
		case <-auto:
			t.toggleAutostart(items)
		case <-quit:
			t.opts.Background.SoftQuit()
		case <-cancelQ:
			t.opts.Background.CancelQuit()
		case <-force:
			t.opts.Background.ForceQuit()
		}
		t.update(items, &view)
	}
}

// trayView is the state last shown in the menu.
type trayView struct {
	status    string
	quitShown bool
}

// update shows the current state in the menu.
func (t *Tray) update(items *trayItems, view *trayView) {
	if items == nil {
		return
	}
	bg := t.opts.Background
	if s := bg.Status(); s != view.status {
		items.status.SetTitle(s)
		view.status = s
	}
	if showQuit := !bg.Waiting(); showQuit != view.quitShown {
		if showQuit {
			items.shuttingDown.Hide()
			items.cancelQuit.Hide()
			items.quit.Show()
			systray.SetTooltip(t.texts.Tooltip)
		} else {
			items.quit.Hide()
			items.shuttingDown.Show()
			items.cancelQuit.Show()
			systray.SetTooltip(t.texts.ShuttingDown)
		}
		view.quitShown = showQuit
	}
	if items.autostart != nil {
		if on, err := t.opts.Autostart.Enabled(); err == nil && on != items.autostart.Checked() {
			if on {
				items.autostart.Check()
			} else {
				items.autostart.Uncheck()
			}
		}
	}
}

// toggleAutostart turns the start with Windows on or off.
func (t *Tray) toggleAutostart(items *trayItems) {
	if items == nil || items.autostart == nil {
		return
	}
	on := !items.autostart.Checked()
	if err := t.opts.Autostart.SetEnabled(on); err != nil {
		t.opts.Logger.Warn("could not change the autostart entry", slog.Bool("enabled", on), slog.Any("error", err))
		return
	}
	t.opts.Logger.Info("start with Windows changed", slog.Bool("enabled", on))
}

// stop removes the icon and ends its message loop.
func (t *Tray) stop() {
	for range trayStopTries {
		if t.ready.Load() {
			systray.Quit()
		} else if tid := t.tid.Load(); tid != 0 {
			postQuit(tid)
		}
		select {
		case <-t.runDone:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	// The loop cannot be ended; the process exit removes it.
	t.opts.Logger.Warn("the tray icon's message loop did not stop")
}

// Notify shows a notification (a balloon, or a toast on Windows 10 and later)
// from the tray icon.
func (t *Tray) Notify(title, message string) error {
	if !t.ready.Load() {
		return errTrayNotReady
	}
	hwnd, err := t.window()
	if err != nil {
		return err
	}
	nid := notifyIconData{Wnd: uintptr(hwnd), ID: systrayIconID, Flags: nifInfo, InfoFlags: niifInfo}
	nid.Size = uint32(unsafe.Sizeof(nid)) //nolint:gosec // G103: Win32 structure size.
	copyUTF16(nid.InfoTitle[:], title)
	copyUTF16(nid.Info[:], message)
	r, _, callErr := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid))) //nolint:gosec // G103: Win32 call.
	if r == 0 {
		return fmt.Errorf("Shell_NotifyIcon: %w", callErr)
	}
	return nil
}

// window returns the window of the tray icon: the one of systray's class owned by
// the tray's thread.
func (t *Tray) window() (windows.Handle, error) {
	class, err := windows.UTF16PtrFromString(systrayClass)
	if err != nil {
		return 0, err
	}
	tid := t.tid.Load()
	var prev uintptr
	for {
		h, _, _ := procFindWindowExW.Call(0, prev, uintptr(unsafe.Pointer(class)), 0) //nolint:gosec // G103: Win32 call.
		if h == 0 {
			return 0, errTrayNotReady
		}
		if owner, _ := windows.GetWindowThreadProcessId(windows.HWND(h), nil); owner == tid {
			return windows.Handle(h), nil
		}
		prev = h
	}
}

// copyUTF16 copies s into dst as a NUL-terminated UTF-16 string, cut to fit.
func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil || len(dst) == 0 {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
