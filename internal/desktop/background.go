package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Background mode (Windows): closing the window hides it and the app keeps running
// in the tray, so scheduled backups continue. Background holds the platform-neutral
// part, the quit logic and the texts; the tray, the registry and the message box
// are behind the functions in BackgroundOptions.

// HiddenFlag starts the desktop app with its window hidden in the tray. The
// Windows autostart entry passes it.
const HiddenFlag = "--hidden"

// ForceQuitReason is recorded as the error message of the backups and restores
// cancelled by "Force quit".
const ForceQuitReason = "cancelled: application force quit"

// BackgroundNoticeFile, in the data directory, records that the "keeps running in
// the background" notice was shown, so it is shown once.
const BackgroundNoticeFile = "background-notice-shown"

// ErrQuitCancelled is the cause of a soft quit's wait for the running backups
// and restores ended by "Cancel quit", "Force quit" or Background.Close.
var ErrQuitCancelled = errors.New("quit cancelled")

// ParseHidden removes HiddenFlag (also written -hidden) from args and reports
// whether it was given. A version started after an update (afterUpdate) always
// shows its window, so ParseHidden reports false then.
func ParseHidden(args []string, afterUpdate bool) (bool, []string) {
	hidden := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == HiddenFlag || a == "-hidden" {
			hidden = true
			continue
		}
		rest = append(rest, a)
	}
	return hidden && !afterUpdate, rest
}

// Language selects the texts of the tray.
type Language string

// Languages of the tray.
const (
	English Language = "en"
	Turkish Language = "tr"
)

// langTurkish is the primary language ID of Turkish in a Windows LANGID.
const langTurkish = 0x1f

// LanguageFromLangID returns Turkish for a Windows LANGID whose primary language
// is Turkish, such as the one GetUserDefaultUILanguage returns, and English
// otherwise.
func LanguageFromLangID(id uint16) Language {
	if id&0x3ff == langTurkish {
		return Turkish
	}
	return English
}

// TrayTexts are the localized texts of the tray menu and its notice.
type TrayTexts struct {
	Tooltip     string
	Open        string
	Autostart   string
	Quit        string
	CancelQuit  string
	ForceQuit   string
	NoticeTitle string
	Notice      string

	lang Language
}

// TextsFor returns the tray texts in lang; any language but Turkish gets English.
func TextsFor(lang Language) TrayTexts {
	if lang == Turkish {
		return TrayTexts{
			Tooltip:     "MongoRescue",
			Open:        "MongoRescue'yu aç",
			Autostart:   "Windows açılışında başlat",
			Quit:        "Çık",
			CancelQuit:  "Çıkışı iptal et",
			ForceQuit:   "Zorla çık",
			NoticeTitle: "MongoRescue",
			Notice:      "MongoRescue arka planda çalışmaya devam ediyor",
			lang:        Turkish,
		}
	}
	return TrayTexts{
		Tooltip:     "MongoRescue",
		Open:        "Open MongoRescue",
		Autostart:   "Start with Windows",
		Quit:        "Quit",
		CancelQuit:  "Cancel quit",
		ForceQuit:   "Force quit",
		NoticeTitle: "MongoRescue",
		Notice:      "MongoRescue keeps running in the background",
		lang:        English,
	}
}

// RunCounts counts the backups and restores among the concurrency keys of the
// running operations (runs.BackupKey, runs.RestoreKey).
func RunCounts(keys []string) (backups, restores int) {
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "backup:"):
			backups++
		case strings.HasPrefix(k, "restore:"):
			restores++
		}
	}
	return backups, restores
}

// runPhrase returns "1 backup", "2 backups and 1 restore" and so on; with running,
// "1 running backup". Turkish has no plural after a number and puts "Çalışan" in
// front of the phrase instead.
func (t TrayTexts) runPhrase(backups, restores int, running bool) string {
	var parts []string
	if t.lang == Turkish {
		if backups > 0 {
			parts = append(parts, fmt.Sprintf("%d yedekleme", backups))
		}
		if restores > 0 {
			parts = append(parts, fmt.Sprintf("%d geri yükleme", restores))
		}
		return strings.Join(parts, " ve ")
	}
	noun := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	}
	prefix := ""
	if running {
		prefix = "running "
	}
	if backups > 0 {
		parts = append(parts, fmt.Sprintf("%d %s%s", backups, prefix, noun(backups, "backup", "backups")))
		prefix = ""
	}
	if restores > 0 {
		parts = append(parts, fmt.Sprintf("%d %s%s", restores, prefix, noun(restores, "restore", "restores")))
	}
	return strings.Join(parts, " and ")
}

// Idle is the status line while nothing runs.
func (t TrayTexts) Idle() string {
	if t.lang == Turkish {
		return "Boşta"
	}
	return "Idle"
}

// Quitting is the status line once the app quits.
func (t TrayTexts) Quitting() string {
	if t.lang == Turkish {
		return "Çıkılıyor…"
	}
	return "Quitting…"
}

// Running is the status line while backups or restores run, such as
// "Running: 1 backup".
func (t TrayTexts) Running(backups, restores int) string {
	if backups+restores == 0 {
		return t.Idle()
	}
	if t.lang == Turkish {
		return "Çalışıyor: " + strings.ReplaceAll(t.runPhrase(backups, restores, false), " ve ", ", ")
	}
	return "Running: " + strings.ReplaceAll(t.runPhrase(backups, restores, false), " and ", ", ")
}

// QuittingAfter is the status line while a soft quit waits, such as "Quitting
// after 1 running backup finishes…".
func (t TrayTexts) QuittingAfter(backups, restores int) string {
	return t.after(backups, restores, "Quitting", "çıkılacak")
}

// UpdatingAfter is the status line while an update waits for the running
// backups and restores.
func (t TrayTexts) UpdatingAfter(backups, restores int) string {
	return t.after(backups, restores, "Updating", "güncellenecek")
}

// after builds QuittingAfter and UpdatingAfter.
func (t TrayTexts) after(backups, restores int, en, tr string) string {
	if backups+restores == 0 {
		return t.Quitting()
	}
	if t.lang == Turkish {
		return "Çalışan " + t.runPhrase(backups, restores, false) + " bittikten sonra " + tr + "…"
	}
	verb := "finish"
	if backups+restores == 1 {
		verb = "finishes"
	}
	return en + " after " + t.runPhrase(backups, restores, true) + " " + verb + "…"
}

// ForceQuitPrompt asks to confirm a force quit that cancels the running backups
// and restores, such as "1 backup is running and will be cancelled. Quit anyway?".
func (t TrayTexts) ForceQuitPrompt(backups, restores int) string {
	if t.lang == Turkish {
		return t.runPhrase(backups, restores, false) + " çalışıyor ve iptal edilecek. Yine de çıkılsın mı?"
	}
	verb := "are"
	if backups+restores == 1 {
		verb = "is"
	}
	return t.runPhrase(backups, restores, false) + " " + verb + " running and will be cancelled. Quit anyway?"
}

// RunController is the part of the application that Background drives;
// *app.App implements it.
type RunController interface {
	// ActiveRuns returns the concurrency keys of the running backups and restores.
	ActiveRuns() []string
	// Busy reports whether a backup or restore runs.
	Busy() bool
	// PauseScheduling keeps scheduled runs from starting.
	PauseScheduling()
	// ResumeScheduling lets scheduled runs start again.
	ResumeScheduling()
}

// defaultQuitPoll is how often a soft quit checks again whether the runs ended.
const defaultQuitPoll = time.Second

// BackgroundOptions configures NewBackground. Runs, Quit and ForceQuit are
// required.
type BackgroundOptions struct {
	// Runs reports the running backups and restores and pauses scheduling.
	Runs RunController
	// Language selects the texts.
	Language Language
	// Quit closes the app normally; it must not block on Background.
	Quit func()
	// ForceQuit cancels the running backups and restores, recording
	// ForceQuitReason, and closes the app; it must not block on Background.
	ForceQuit func()
	// Confirm asks the user to confirm message and reports the answer; it returns
	// false once ctx ends. Nil confirms.
	Confirm func(ctx context.Context, title, message string) bool
	// HideWindow hides the window; nil does nothing.
	HideWindow func()
	// Notify shows a notification from the tray icon; nil does nothing.
	Notify func(title, message string) error
	// NoticeFile records that the background notice was shown; "" shows it once
	// per process.
	NoticeFile string
	// UpdateWaiting reports whether a downloaded update waits for the running
	// backups and restores (UpdateWaiting); the status line then says so. Nil
	// means never.
	UpdateWaiting func() bool
	// Changed is called after the state shown in the tray menu changed; nil does
	// nothing. It must not block on Background.
	Changed func()
	// Poll is how often a soft quit checks again whether the runs ended; 0 means
	// one second.
	Poll time.Duration
	// Logger receives the log records; nil means slog.Default().
	Logger *slog.Logger
}

// Background implements the background mode: the soft quit, which waits for the
// running backups and restores, the force quit, which cancels them, and hiding
// the window on close. Its goroutines end with Close. It is safe for concurrent
// use.
type Background struct {
	opts  BackgroundOptions
	texts TrayTexts

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.Mutex
	closed      bool
	quitting    bool
	waitCancel  context.CancelCauseFunc // set while a soft quit waits
	noticeShown bool
}

// NewBackground returns the background mode for opts.
func NewBackground(opts BackgroundOptions) *Background {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultQuitPoll
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Background{opts: opts, texts: TextsFor(opts.Language), ctx: ctx, cancel: cancel}
}

// Texts returns the tray texts.
func (b *Background) Texts() TrayTexts {
	return b.texts
}

// Close ends a soft quit in progress without quitting, closes a confirmation in
// progress and waits for Background's goroutines. Later soft quits do nothing.
func (b *Background) Close() {
	b.mu.Lock()
	b.closed = true
	if b.waitCancel != nil {
		b.waitCancel(ErrQuitCancelled)
	}
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

// Status returns the status line of the tray menu.
func (b *Background) Status() string {
	backups, restores := RunCounts(b.opts.Runs.ActiveRuns())
	b.mu.Lock()
	waiting, quitting := b.waitCancel != nil, b.quitting
	b.mu.Unlock()
	switch {
	case waiting:
		return b.texts.QuittingAfter(backups, restores)
	case quitting:
		return b.texts.Quitting()
	case b.opts.UpdateWaiting != nil && b.opts.UpdateWaiting():
		return b.texts.UpdatingAfter(backups, restores)
	default:
		return b.texts.Running(backups, restores)
	}
}

// Waiting reports whether a soft quit waits for the running backups and
// restores; the tray then offers "Cancel quit" instead of "Quit".
func (b *Background) Waiting() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waitCancel != nil
}

// Quitting reports whether the app quits (Quit or ForceQuit was called).
func (b *Background) Quitting() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quitting
}

// SoftQuit quits now when no backup or restore runs. Otherwise it keeps scheduled
// runs from starting and quits once the running ones have finished, however long
// that takes; CancelQuit stops the wait and resumes scheduling.
func (b *Background) SoftQuit() {
	b.mu.Lock()
	if b.closed || b.quitting || b.waitCancel != nil {
		b.mu.Unlock()
		return
	}
	if !b.opts.Runs.Busy() {
		b.mu.Unlock()
		b.quit()
		return
	}
	ctx, cancel := context.WithCancelCause(b.ctx)
	b.waitCancel = cancel
	// Paused under b.mu, so CancelQuit cannot resume before the pause.
	b.opts.Runs.PauseScheduling()
	b.wg.Add(1)
	b.mu.Unlock()

	backups, restores := RunCounts(b.opts.Runs.ActiveRuns())
	b.opts.Logger.Info("quitting once the running backups and restores finish",
		slog.Int("backups", backups), slog.Int("restores", restores))
	b.changed()
	go func() {
		defer b.wg.Done()
		err := WaitIdle(ctx, b.opts.Runs.Busy, b.opts.Poll)
		b.mu.Lock()
		b.waitCancel = nil
		if err != nil && !b.quitting {
			b.opts.Runs.ResumeScheduling()
		}
		b.mu.Unlock()
		cancel(nil)
		if err != nil {
			b.opts.Logger.Info("soft quit cancelled", slog.Any("reason", context.Cause(ctx)))
			b.changed()
			return
		}
		b.quit()
	}()
}

// CancelQuit stops a soft quit waiting for the running backups and restores and
// lets scheduled runs start again.
func (b *Background) CancelQuit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waitCancel != nil {
		b.waitCancel(ErrQuitCancelled)
	}
}

// ForceQuit quits now. When backups or restores run, it first asks to confirm
// that they are cancelled; ForceQuit in BackgroundOptions records them as
// cancelled with ForceQuitReason.
func (b *Background) ForceQuit() {
	backups, restores := RunCounts(b.opts.Runs.ActiveRuns())
	if backups+restores > 0 && b.opts.Confirm != nil &&
		!b.opts.Confirm(b.ctx, b.texts.NoticeTitle, b.texts.ForceQuitPrompt(backups, restores)) {
		return
	}
	b.mu.Lock()
	if b.quitting || b.closed {
		b.mu.Unlock()
		return
	}
	b.quitting = true
	if b.waitCancel != nil {
		b.waitCancel(ErrQuitCancelled)
	}
	b.mu.Unlock()
	b.opts.Logger.Warn("force quit", slog.Int("backups", backups), slog.Int("restores", restores))
	b.changed()
	b.opts.ForceQuit()
}

// WindowClosing hides the window instead of closing it. The first time, it shows
// a notice that the app keeps running, and records that in NoticeFile.
func (b *Background) WindowClosing() {
	if b.opts.HideWindow != nil {
		b.opts.HideWindow()
	}
	b.mu.Lock()
	shown := b.noticeShown
	b.noticeShown = true
	b.mu.Unlock()
	if shown || b.opts.Notify == nil {
		return
	}
	if b.opts.NoticeFile != "" {
		if _, err := os.Stat(b.opts.NoticeFile); err == nil {
			return
		}
	}
	if err := b.opts.Notify(b.texts.NoticeTitle, b.texts.Notice); err != nil {
		b.opts.Logger.Warn("could not show the background notice", slog.Any("error", err))
		return
	}
	if b.opts.NoticeFile == "" {
		return
	}
	if err := os.WriteFile(b.opts.NoticeFile, []byte("1\n"), 0o600); err != nil {
		b.opts.Logger.Warn("could not record the background notice", slog.String("path", b.opts.NoticeFile), slog.Any("error", err))
	}
}

// quit quits normally.
func (b *Background) quit() {
	b.mu.Lock()
	if b.quitting {
		b.mu.Unlock()
		return
	}
	b.quitting = true
	b.mu.Unlock()
	b.changed()
	b.opts.Quit()
}

// changed reports a change of the menu state.
func (b *Background) changed() {
	if b.opts.Changed != nil {
		b.opts.Changed()
	}
}
