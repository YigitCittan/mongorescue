package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRuns is a RunController with settable runs.
type fakeRuns struct {
	mu      sync.Mutex
	keys    []string
	paused  bool
	pauses  int
	resumes int
}

func (f *fakeRuns) ActiveRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

func (f *fakeRuns) Busy() bool {
	return len(f.ActiveRuns()) > 0
}

func (f *fakeRuns) PauseScheduling() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	f.pauses++
}

func (f *fakeRuns) ResumeScheduling() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
	f.resumes++
}

func (f *fakeRuns) set(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeRuns) state() (paused bool, pauses, resumes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused, f.pauses, f.resumes
}

// quitRecorder counts the quits.
type quitRecorder struct {
	soft, force atomic.Int32
	softCh      chan struct{}
}

func newBackgroundForTest(t *testing.T, runs *fakeRuns, opts BackgroundOptions) (*Background, *quitRecorder) {
	t.Helper()
	q := &quitRecorder{softCh: make(chan struct{}, 1)}
	opts.Runs = runs
	opts.Quit = func() {
		q.soft.Add(1)
		q.softCh <- struct{}{}
	}
	opts.ForceQuit = func() { q.force.Add(1) }
	opts.Poll = 5 * time.Millisecond
	b := NewBackground(opts)
	t.Cleanup(b.Close)
	return b, q
}

func waitQuit(t *testing.T, q *quitRecorder) {
	t.Helper()
	select {
	case <-q.softCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the app did not quit")
	}
}

func TestSoftQuitWithoutRunsQuitsNow(t *testing.T) {
	runs := &fakeRuns{}
	b, q := newBackgroundForTest(t, runs, BackgroundOptions{})
	b.SoftQuit()
	waitQuit(t, q)
	if paused, _, _ := runs.state(); paused {
		t.Fatal("scheduling paused for an idle quit")
	}
	if !b.Quitting() || b.Status() != "Quitting…" {
		t.Fatalf("Quitting()=%v Status()=%q", b.Quitting(), b.Status())
	}
	b.SoftQuit()
	if q.soft.Load() != 1 {
		t.Fatalf("quit %d times; want 1", q.soft.Load())
	}
}

func TestSoftQuitWaitsForTheRunsThenQuits(t *testing.T) {
	runs := &fakeRuns{keys: []string{"backup:c/shop"}}
	var changes atomic.Int32
	b, q := newBackgroundForTest(t, runs, BackgroundOptions{Changed: func() { changes.Add(1) }})
	b.SoftQuit()
	if !b.Waiting() {
		t.Fatal("Waiting() = false while the backup runs")
	}
	if paused, _, _ := runs.state(); !paused {
		t.Fatal("scheduling not paused while the soft quit waits")
	}
	if got, want := b.Status(), "Quitting after 1 running backup finishes…"; got != want {
		t.Fatalf("Status() = %q; want %q", got, want)
	}
	b.SoftQuit() // a second click does not start a second wait
	time.Sleep(30 * time.Millisecond)
	if q.soft.Load() != 0 {
		t.Fatal("quit while the backup still runs")
	}

	runs.set()
	waitQuit(t, q)
	if b.Waiting() || !b.Quitting() {
		t.Fatalf("Waiting()=%v Quitting()=%v after the quit", b.Waiting(), b.Quitting())
	}
	if paused, _, resumes := runs.state(); !paused || resumes != 0 {
		t.Fatalf("scheduling paused=%v resumes=%d; it must stay paused for the quit", paused, resumes)
	}
	if changes.Load() == 0 {
		t.Fatal("Changed was not called")
	}
	if q.soft.Load() != 1 || q.force.Load() != 0 {
		t.Fatalf("soft=%d force=%d; want 1/0", q.soft.Load(), q.force.Load())
	}
}

func TestCancelQuitResumesScheduling(t *testing.T) {
	runs := &fakeRuns{keys: []string{"backup:c/shop", "restore:c/shop_rescue"}}
	b, q := newBackgroundForTest(t, runs, BackgroundOptions{})
	b.SoftQuit()
	if got, want := b.Status(), "Quitting after 1 running backup and 1 restore finish…"; got != want {
		t.Fatalf("Status() = %q; want %q", got, want)
	}
	b.CancelQuit()
	deadline := time.Now().Add(2 * time.Second)
	for b.Waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the soft quit still waits after CancelQuit")
		}
		time.Sleep(time.Millisecond)
	}
	if paused, pauses, resumes := runs.state(); paused || pauses != 1 || resumes != 1 {
		t.Fatalf("paused=%v pauses=%d resumes=%d; want resumed", paused, pauses, resumes)
	}
	runs.set()
	time.Sleep(30 * time.Millisecond)
	if q.soft.Load() != 0 || b.Quitting() {
		t.Fatal("a cancelled soft quit quit anyway")
	}
	if got := b.Status(); got != "Idle" {
		t.Fatalf("Status() = %q; want Idle", got)
	}
	// A new soft quit works after the cancel.
	b.SoftQuit()
	waitQuit(t, q)
}

func TestForceQuitConfirmsAndCancelsTheRuns(t *testing.T) {
	runs := &fakeRuns{keys: []string{"backup:c/shop"}}
	var prompts []string
	answer := false
	b, q := newBackgroundForTest(t, runs, BackgroundOptions{
		Confirm: func(_ context.Context, title, message string) bool {
			prompts = append(prompts, title+": "+message)
			return answer
		},
	})
	b.ForceQuit()
	if q.force.Load() != 0 || b.Quitting() {
		t.Fatal("force quit without confirmation")
	}
	want := "MongoRescue: 1 backup is running and will be cancelled. Quit anyway?"
	if len(prompts) != 1 || prompts[0] != want {
		t.Fatalf("prompts = %q; want [%q]", prompts, want)
	}

	// Confirmed during a soft quit: the wait ends without a soft quit.
	b.SoftQuit()
	answer = true
	b.ForceQuit()
	if q.force.Load() != 1 || !b.Quitting() {
		t.Fatalf("force=%d quitting=%v; want 1/true", q.force.Load(), b.Quitting())
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.Waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the soft quit still waits after the force quit")
		}
		time.Sleep(time.Millisecond)
	}
	if paused, _, resumes := runs.state(); !paused || resumes != 0 {
		t.Fatalf("paused=%v resumes=%d; scheduling must stay paused while quitting", paused, resumes)
	}
	if q.soft.Load() != 0 {
		t.Fatal("soft quit after the force quit")
	}
	b.ForceQuit()
	if q.force.Load() != 1 {
		t.Fatal("force quit twice")
	}
}

func TestForceQuitWithoutRunsDoesNotAsk(t *testing.T) {
	b, q := newBackgroundForTest(t, &fakeRuns{}, BackgroundOptions{
		Confirm: func(context.Context, string, string) bool {
			t.Error("asked to confirm without runs")
			return false
		},
	})
	b.ForceQuit()
	if q.force.Load() != 1 {
		t.Fatal("no force quit")
	}
}

func TestCloseEndsTheSoftQuitAndTheConfirmation(t *testing.T) {
	runs := &fakeRuns{keys: []string{"backup:c/shop"}}
	confirmed := make(chan bool, 1)
	b, q := newBackgroundForTest(t, runs, BackgroundOptions{
		Confirm: func(ctx context.Context, _, _ string) bool {
			<-ctx.Done()
			return false
		},
	})
	b.SoftQuit()
	go func() {
		b.ForceQuit()
		confirmed <- true
	}()
	time.Sleep(10 * time.Millisecond)
	b.Close()
	select {
	case <-confirmed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the confirmation")
	}
	if b.Waiting() || q.soft.Load() != 0 || q.force.Load() != 0 {
		t.Fatalf("waiting=%v soft=%d force=%d after Close", b.Waiting(), q.soft.Load(), q.force.Load())
	}
	b.SoftQuit()
	if b.Waiting() {
		t.Fatal("soft quit after Close")
	}
}

func TestWindowClosingHidesAndShowsTheNoticeOnce(t *testing.T) {
	notice := filepath.Join(t.TempDir(), BackgroundNoticeFile)
	var hides, notices atomic.Int32
	var got []string
	opts := BackgroundOptions{
		Language:   Turkish,
		HideWindow: func() { hides.Add(1) },
		Notify: func(title, message string) error {
			notices.Add(1)
			got = append(got, title+": "+message)
			return nil
		},
		NoticeFile: notice,
	}
	b, _ := newBackgroundForTest(t, &fakeRuns{}, opts)
	b.WindowClosing()
	b.WindowClosing()
	if hides.Load() != 2 || notices.Load() != 1 {
		t.Fatalf("hides=%d notices=%d; want 2/1", hides.Load(), notices.Load())
	}
	if want := "MongoRescue: MongoRescue arka planda çalışmaya devam ediyor"; got[0] != want {
		t.Fatalf("notice = %q; want %q", got[0], want)
	}
	if _, err := os.Stat(notice); err != nil {
		t.Fatalf("notice not recorded: %v", err)
	}
	// A later start does not show it again.
	again, _ := newBackgroundForTest(t, &fakeRuns{}, opts)
	again.WindowClosing()
	if hides.Load() != 3 || notices.Load() != 1 {
		t.Fatalf("hides=%d notices=%d after a restart; want 3/1", hides.Load(), notices.Load())
	}
}

func TestWindowClosingRetriesTheNoticeAfterAFailure(t *testing.T) {
	notice := filepath.Join(t.TempDir(), BackgroundNoticeFile)
	opts := BackgroundOptions{
		Notify:     func(string, string) error { return errors.New("no tray") },
		NoticeFile: notice,
	}
	b, _ := newBackgroundForTest(t, &fakeRuns{}, opts)
	b.WindowClosing()
	if _, err := os.Stat(notice); !os.IsNotExist(err) {
		t.Fatalf("a notice that failed was recorded: %v", err)
	}
}

func TestBackgroundStatusLines(t *testing.T) {
	runs := &fakeRuns{}
	updating := false
	b, _ := newBackgroundForTest(t, runs, BackgroundOptions{UpdateWaiting: func() bool { return updating }})
	if got := b.Status(); got != "Idle" {
		t.Fatalf("Status() = %q; want Idle", got)
	}
	runs.set("backup:c/a", "backup:c/b", "restore:c/x", "other")
	if got, want := b.Status(), "Running: 2 backups, 1 restore"; got != want {
		t.Fatalf("Status() = %q; want %q", got, want)
	}
	updating = true
	if got, want := b.Status(), "Updating after 2 running backups and 1 restore finish…"; got != want {
		t.Fatalf("Status() = %q; want %q", got, want)
	}
}

func TestTrayTexts(t *testing.T) {
	en, tr := TextsFor(English), TextsFor(Turkish)
	cases := []struct{ got, want string }{
		{en.Running(1, 0), "Running: 1 backup"},
		{en.Running(0, 2), "Running: 2 restores"},
		{en.QuittingAfter(1, 0), "Quitting after 1 running backup finishes…"},
		{en.QuittingAfter(2, 0), "Quitting after 2 running backups finish…"},
		{en.QuittingAfter(0, 1), "Quitting after 1 running restore finishes…"},
		{en.ForceQuitPrompt(1, 0), "1 backup is running and will be cancelled. Quit anyway?"},
		{en.ForceQuitPrompt(2, 1), "2 backups and 1 restore are running and will be cancelled. Quit anyway?"},
		{tr.Running(1, 1), "Çalışıyor: 1 yedekleme, 1 geri yükleme"},
		{tr.Idle(), "Boşta"},
		{tr.QuittingAfter(1, 0), "Çalışan 1 yedekleme bittikten sonra çıkılacak…"},
		{tr.ForceQuitPrompt(1, 0), "1 yedekleme çalışıyor ve iptal edilecek. Yine de çıkılsın mı?"},
		{tr.Open, "MongoRescue'yu aç"},
		{tr.Autostart, "Windows açılışında başlat"},
		{tr.Quit, "Çık"},
		{tr.CancelQuit, "Çıkışı iptal et"},
		{tr.ForceQuit, "Zorla çık"},
		{en.Open, "Open MongoRescue"},
		{en.Autostart, "Start with Windows"},
		{TextsFor("de").Quit, "Quit"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q; want %q", c.got, c.want)
		}
	}
}

func TestLanguageFromLangID(t *testing.T) {
	for id, want := range map[uint16]Language{0x041f: Turkish, 0x001f: Turkish, 0x0409: English, 0x0407: English, 0: English} {
		if got := LanguageFromLangID(id); got != want {
			t.Errorf("LanguageFromLangID(%#x) = %s; want %s", id, got, want)
		}
	}
}

func TestParseHidden(t *testing.T) {
	hidden, rest := ParseHidden([]string{"-log-level", "debug", HiddenFlag}, false)
	if !hidden || !slices.Equal(rest, []string{"-log-level", "debug"}) {
		t.Fatalf("ParseHidden = %v %q", hidden, rest)
	}
	if hidden, rest = ParseHidden([]string{"-hidden"}, false); !hidden || len(rest) != 0 {
		t.Fatalf("ParseHidden(-hidden) = %v %q", hidden, rest)
	}
	if hidden, _ = ParseHidden([]string{"-data-dir", "x"}, false); hidden {
		t.Fatal("hidden without the flag")
	}
	// A relaunch after an update shows the window, even from a hidden start.
	if hidden, rest = ParseHidden([]string{HiddenFlag}, true); hidden || len(rest) != 0 {
		t.Fatalf("ParseHidden after an update = %v %q; want shown, flag removed", hidden, rest)
	}
}

// fakeRunKey is an in-memory RunKey.
type fakeRunKey struct {
	values map[string]string
	err    error
}

func (k *fakeRunKey) Get(name string) (string, bool, error) {
	v, ok := k.values[name]
	return v, ok, k.err
}

func (k *fakeRunKey) Set(name, value string) error {
	if k.err != nil {
		return k.err
	}
	k.values[name] = value
	return nil
}

func (k *fakeRunKey) Delete(name string) error {
	delete(k.values, name)
	return k.err
}

func TestAutostartCommandQuoting(t *testing.T) {
	cases := map[string]string{
		`C:\Users\Ada Lovelace\AppData\Local\Programs\MongoRescue\MongoRescue.exe`: `"C:\Users\Ada Lovelace\AppData\Local\Programs\MongoRescue\MongoRescue.exe" --hidden`,
		`C:\MongoRescue.exe`: `"C:\MongoRescue.exe" --hidden`,
	}
	for exe, want := range cases {
		got, err := AutostartCommand(exe)
		if err != nil || got != want {
			t.Errorf("AutostartCommand(%q) = %q, %v; want %q", exe, got, err, want)
		}
	}
	for _, bad := range []string{"", `C:\a"b.exe`, "C:\\a\nb.exe"} {
		if _, err := AutostartCommand(bad); !errors.Is(err, ErrInvalidExecutable) {
			t.Errorf("AutostartCommand(%q) error = %v; want ErrInvalidExecutable", bad, err)
		}
	}
}

func TestAutostartReflectsAndChangesTheEntry(t *testing.T) {
	key := &fakeRunKey{values: map[string]string{}}
	a := &Autostart{Key: key, Executable: func() (string, error) { return `C:\Apps\Mongo Rescue\MongoRescue.exe`, nil }}
	if on, err := a.Enabled(); err != nil || on {
		t.Fatalf("Enabled() = %v, %v; want false", on, err)
	}
	if err := a.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if got, want := key.values[AutostartValueName], `"C:\Apps\Mongo Rescue\MongoRescue.exe" --hidden`; got != want {
		t.Fatalf("Run value = %q; want %q", got, want)
	}
	if on, err := a.Enabled(); err != nil || !on {
		t.Fatalf("Enabled() = %v, %v; want true", on, err)
	}
	if err := a.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, ok := key.values[AutostartValueName]; ok {
		t.Fatal("Run value not removed")
	}
	// An entry written elsewhere (by the installer, another copy) is reflected.
	key.values[AutostartValueName] = `"D:\MongoRescue.exe" --hidden`
	if on, _ := a.Enabled(); !on {
		t.Fatal("an existing entry is not reflected")
	}
	key.err = errors.New("access denied")
	if _, err := a.Enabled(); err == nil {
		t.Fatal("Enabled() hid a registry error")
	}
	if err := a.SetEnabled(true); err == nil {
		t.Fatal("SetEnabled() hid a registry error")
	}
}
