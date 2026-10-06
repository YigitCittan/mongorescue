// Package runlog persists the log of every backup and restore run as a text file
// under <datadir>/logs/<run-id>.log: the complete output of mongodump/mongorestore
// and MongoRescue's own phase lines for that run.
//
// Every line is redacted before it reaches the disk (connection strings and secrets
// through internal/redact, document values in duplicate-key lines masked) and cut to
// a bounded length, like mongotools.QuoteTail does for error messages. A file never
// grows beyond MaxFileBytes: the first HeadBytes are kept, then the most recent output
// (at least TailBytes/2, at most TailBytes) after a "… N bytes truncated …" marker.
// Output streams to disk; a Writer holds at most one partial line in memory.
package runlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Size limits of a run log.
const (
	// MaxFileBytes caps a finished log file.
	MaxFileBytes = 5 << 20
	// HeadBytes is how much of the beginning of a run's output is always kept.
	HeadBytes = 1 << 20
	// TailBytes is the most of the end of a run's output that is kept once the head
	// is full.
	TailBytes = MaxFileBytes - HeadBytes - markerReserve
	// ToolLineMax bounds each line of tool output, as mongotools.QuotedLineMax bounds
	// quoted stderr lines, so document values are not stored at length.
	ToolLineMax = mongotools.QuotedLineMax
	// MessageLineMax bounds each of MongoRescue's own lines.
	MessageLineMax = 4 << 10
	// MaxTailLines caps the lines Tail returns.
	MaxTailLines = 10000
)

const (
	// markerReserve is room for the truncation marker and one overlong line.
	markerReserve = 16 << 10
	// segmentBytes is the size at which the tail moves on to a new segment file.
	segmentBytes = TailBytes/2 - MessageLineMax - 1
	// pendingMax bounds a partial tool line held in memory before it is cut.
	pendingMax = 64 << 10
	// dirPerm and filePerm keep logs private to the service user.
	dirPerm  = 0o700
	filePerm = 0o600
	// extension is the suffix of a log file.
	extension = ".log"
	// segmentInfix separates a log file name from a tail segment number.
	segmentInfix = ".seg."
)

// ellipsis marks a cut line.
const ellipsis = "…"

// Sentinel errors.
var (
	// ErrNotFound is returned when a run has no log file.
	ErrNotFound = errors.New("runlog: no log for this run")
	// ErrInvalidID is returned for a run ID that cannot name a file.
	ErrInvalidID = errors.New("runlog: invalid run id")
	// ErrClosed is returned by writes after Close.
	ErrClosed = errors.New("runlog: writer closed")
	// ErrInvalidPath is returned for a log file path outside the log directory.
	ErrInvalidPath = errors.New("runlog: log path outside the log directory")
)

// Dir is the directory holding the run logs. The zero value is not usable; call
// NewDir.
type Dir struct {
	path string
	now  func() time.Time
}

// NewDir returns the log directory at path, made absolute; it is created on the
// first write.
func NewDir(path string) *Dir {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return &Dir{path: filepath.Clean(path), now: time.Now}
}

// Path returns the directory.
func (d *Dir) Path() string { return d.path }

// file returns the path of the log of run id.
func (d *Dir) file(id string) (string, error) {
	if err := models.ValidateID(id); err != nil && !legacyID(id) {
		return "", fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return checkedPath(d.path, filepath.Join(d.path, id+extension))
}

// maxLegacyIDLength bounds a legacy run ID (a file name).
const maxLegacyIDLength = 255

// legacyIDPattern matches the IDs of older records, which may embed database names:
// one file name element that does not start with a dot (so never "." or ".."), with
// no path separator, drive colon or control character.
var legacyIDPattern = regexp.MustCompile(`^[^./\\:\pC][^/\\:\pC]*$`)

// legacyID accepts the IDs of older records as long as they cannot leave the
// directory.
func legacyID(id string) bool {
	return len(id) <= maxLegacyIDLength && utf8.ValidString(id) && legacyIDPattern.MatchString(id)
}

// dotDotElement matches a path with a ".." element (separated by / or \); names that
// merely contain two dots, such as "v1..v2", are fine.
var dotDotElement = regexp.MustCompile(`(?:^|[/\\])\.\.(?:[/\\]|$)`)

// checkedPath is the last check of a log file path before it reaches the file
// system: no control character, no ".." element, cleaned, absolute and inside dir
// (the absolute log directory). Run IDs are validated when the path is built; this
// makes sure the value handed to the file system is still what validation saw.
func checkedPath(dir, p string) (string, error) {
	if strings.ContainsFunc(p, unicode.IsControl) || dotDotElement.MatchString(p) {
		return "", ErrInvalidPath
	}
	p = filepath.Clean(p)
	prefix := dir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if !filepath.IsAbs(p) || !strings.HasPrefix(p, prefix) {
		return "", ErrInvalidPath
	}
	return p, nil
}

// Remove deletes the log of run id (and any tail segment left by a crash). A missing
// log is not an error; a file still open elsewhere (on Windows) is retried briefly and
// then reported, so callers log it and carry on (see removeFile).
func (d *Dir) Remove(id string) error {
	path, err := d.file(id)
	if err != nil {
		return err
	}
	var errs []error
	if err := removeFile(path); err != nil {
		errs = append(errs, err)
	}
	segs, _ := filepath.Glob(globEscape(path) + segmentInfix + "*")
	for _, s := range segs {
		seg, err := checkedPath(d.path, s)
		if err == nil {
			err = removeFile(seg)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeAttempts and removeBackoff bound the retries of removeFile.
const (
	removeAttempts = 5
	removeBackoff  = 20 * time.Millisecond
)

// removeFile deletes path; a missing file is not an error. Windows cannot delete a
// file that is open (a log being downloaded or followed), so a failed removal is
// retried a few times with a growing backoff (20 ms to 160 ms) before its error is
// returned for the caller to log; the file is then left for the next prune.
func removeFile(path string) error {
	var err error
	for attempt, wait := 0, removeBackoff; attempt < removeAttempts; attempt, wait = attempt+1, wait*2 {
		if err = os.Remove(path); err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if attempt < removeAttempts-1 {
			time.Sleep(wait)
		}
	}
	return fmt.Errorf("runlog: remove %s: %w", filepath.Base(path), err)
}

// Prune deletes log files (and stray segments) last modified more than keep ago and
// returns how many it removed. Files it cannot remove (still open, on Windows) are
// skipped and reported in the joined error; the next prune tries again. A keep of zero or less removes nothing; skip, when
// set, protects the logs of runs that are still active.
func (d *Dir) Prune(keep time.Duration, skip func(id string) bool) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("runlog: read %s: %w", d.path, err)
	}
	cutoff := d.now().Add(-keep)
	removed := 0
	var errs []error
	for _, e := range entries {
		name := e.Name()
		id, ok := runIDOf(name)
		if !ok || e.IsDir() || (skip != nil && skip(id)) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path, err := checkedPath(d.path, filepath.Join(d.path, name))
		if err == nil {
			err = removeFile(path)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// runIDOf returns the run ID of a log or segment file name.
func runIDOf(name string) (string, bool) {
	if i := strings.Index(name, extension+segmentInfix); i > 0 {
		return name[:i], true
	}
	if id, ok := strings.CutSuffix(name, extension); ok && id != "" {
		return id, true
	}
	return "", false
}

// globEscape escapes the glob metacharacters of a path.
func globEscape(path string) string {
	r := strings.NewReplacer(`[`, `\[`, `]`, `\]`, `*`, `\*`, `?`, `\?`)
	if os.PathSeparator == '\\' {
		// filepath.Match has no escaping on Windows; IDs never contain these.
		return path
	}
	return r.Replace(path)
}

// Create starts the log of run id, replacing an older log of the same ID.
func (d *Dir) Create(id string) (*Writer, error) {
	path, err := d.file(id)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(d.path, dirPerm); err != nil {
		return nil, fmt.Errorf("runlog: create %s: %w", d.path, err)
	}
	_ = d.Remove(id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return nil, fmt.Errorf("runlog: create log: %w", err)
	}
	return &Writer{dir: d.path, path: path, head: f, now: d.now}, nil
}

// Open returns a reader over the finished log of run id, or ErrNotFound.
func (d *Dir) Open(id string) (*Reader, error) {
	path, err := d.file(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runlog: open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("runlog: stat log: %w", err)
	}
	return &Reader{parts: []part{{r: f, size: info.Size()}}, closers: []io.Closer{f}}, nil
}

// segment is one tail file of a Writer.
type segment struct {
	path string
	f    *os.File
	size int64
}

// Writer writes the log of one run. Write accepts tool output (split into lines,
// redacted and cut to ToolLineMax); Printf adds MongoRescue's own timestamped lines.
// It is safe for concurrent use. The file is complete once Close returns.
type Writer struct {
	dir  string // the absolute log directory
	path string
	now  func() time.Time

	mu       sync.Mutex
	head     *os.File
	headSize int64
	segs     []segment // at most two: the previous (full) and the current one
	nextSeg  int
	dropped  int64
	stale    []string // segment files whose removal failed (open on Windows)
	pending  []byte   // partial tool line
	cut      bool     // pending exceeded pendingMax and is being discarded
	closed   bool
	err      error
}

// Write implements io.Writer for tool output: complete lines are redacted, cut and
// stored; a trailing partial line waits for its newline (or Close). Write never fails
// because of the disk: a write error is kept and reported by Close.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buffer(p)
			break
		}
		w.buffer(p[:i])
		w.flushPending()
		p = p[i+1:]
	}
	return n, nil
}

// buffer appends part of a line to pending, bounded by pendingMax.
func (w *Writer) buffer(p []byte) {
	if w.cut {
		return
	}
	if room := pendingMax - len(w.pending); len(p) > room {
		p = p[:room]
		w.cut = true
	}
	w.pending = append(w.pending, p...)
}

// flushPending stores the pending tool line.
func (w *Writer) flushPending() {
	line := strings.TrimRight(string(w.pending), "\r")
	w.pending, w.cut = w.pending[:0], false
	if strings.TrimSpace(line) == "" {
		return
	}
	w.writeLine(ToolLine(line))
}

// Printf adds one of MongoRescue's own lines, prefixed with the current time.
func (w *Writer) Printf(format string, args ...any) {
	if w == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	stamp := w.now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	for _, l := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		w.writeLine(stamp + "\t[mongorescue] " + MessageLine(l))
	}
}

// writeLine stores one redacted line (without its newline). Caller holds w.mu.
func (w *Writer) writeLine(line string) {
	if w.err != nil {
		return
	}
	b := []byte(line + "\n")
	if len(w.segs) == 0 && w.headSize+int64(len(b)) <= HeadBytes {
		w.write(w.head, b, &w.headSize)
		return
	}
	cur := w.current()
	if cur == nil {
		return
	}
	w.write(cur.f, b, &cur.size)
}

// current returns the segment receiving tail lines, starting a new one when the
// current one is full. Caller holds w.mu.
func (w *Writer) current() *segment {
	if n := len(w.segs); n > 0 && w.segs[n-1].size < segmentBytes {
		return &w.segs[n-1]
	}
	if len(w.segs) == 2 {
		old := w.segs[0]
		w.dropped += old.size
		_ = old.f.Close()
		w.removeSegment(old.path)
		w.segs = w.segs[1:]
	}
	w.retryStale()
	path, err := checkedPath(w.dir, w.path+segmentInfix+strconv.Itoa(w.nextSeg))
	if err != nil {
		w.err = fmt.Errorf("runlog: create tail segment: %w", err)
		return nil
	}
	w.nextSeg++
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		w.err = fmt.Errorf("runlog: create tail segment: %w", err)
		return nil
	}
	w.segs = append(w.segs, segment{path: path, f: f})
	return &w.segs[len(w.segs)-1]
}

// removeSegment deletes a tail segment file, remembering it for retryStale when the
// removal fails (the file is open elsewhere, on Windows). Caller holds w.mu.
func (w *Writer) removeSegment(p string) {
	if !w.removeChecked(p) {
		w.stale = append(w.stale, p)
	}
}

// removeChecked deletes the segment file at p, which must lie in the log directory,
// and reports whether it is gone. Caller holds w.mu.
func (w *Writer) removeChecked(p string) bool {
	path, err := checkedPath(w.dir, p)
	if err != nil {
		return true // never a file of this writer: nothing to retry
	}
	err = os.Remove(path)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// retryStale removes segment files whose earlier removal failed. Caller holds w.mu.
func (w *Writer) retryStale() {
	kept := w.stale[:0]
	for _, p := range w.stale {
		if !w.removeChecked(p) {
			kept = append(kept, p)
		}
	}
	w.stale = kept
}

// write appends b to f and advances *size. Caller holds w.mu.
func (w *Writer) write(f *os.File, b []byte, size *int64) {
	n, err := f.Write(b)
	*size += int64(n)
	if err != nil {
		w.err = fmt.Errorf("runlog: write log: %w", err)
	}
}

// Close stores a pending partial line, appends the kept tail (after the truncation
// marker) to the file and removes the tail segments. It is idempotent and returns the
// first write error.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.err
	}
	if len(w.pending) > 0 {
		w.flushPending()
	}
	w.closed = true
	if len(w.segs) > 0 && w.err == nil {
		if _, err := w.head.WriteString(marker(w.dropped)); err != nil {
			w.err = fmt.Errorf("runlog: write log: %w", err)
		}
	}
	for _, s := range w.segs {
		_ = s.f.Close()
		if w.err == nil {
			w.err = copySegment(w.head, w.dir, s.path)
		}
		w.removeSegment(s.path)
	}
	w.segs = nil
	w.retryStale()
	if err := w.head.Close(); err != nil && w.err == nil {
		w.err = fmt.Errorf("runlog: close log: %w", err)
	}
	return w.err
}

// copySegment appends the segment file at path, inside dir, to dst.
func copySegment(dst io.Writer, dir, path string) error {
	path, err := checkedPath(dir, path)
	if err != nil {
		return fmt.Errorf("runlog: read tail segment: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("runlog: read tail segment: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(dst, f); err != nil {
		return fmt.Errorf("runlog: append tail segment: %w", err)
	}
	return nil
}

// marker is the line between the head and the tail of a capped log.
func marker(dropped int64) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf("%s %d bytes truncated %s\n", ellipsis, dropped, ellipsis)
}

// Reader opens the log as written so far: the head, the truncation marker and the
// tail segments, as one stream. The caller must Close it.
func (w *Writer) Reader() (*Reader, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, ErrClosed
	}
	r := &Reader{}
	add := func(p string, size int64) error {
		path, err := checkedPath(w.dir, p)
		if err != nil {
			_ = r.Close()
			return fmt.Errorf("runlog: open log: %w", err)
		}
		f, err := os.Open(path)
		if err != nil {
			_ = r.Close()
			return fmt.Errorf("runlog: open log: %w", err)
		}
		r.closers = append(r.closers, f)
		r.parts = append(r.parts, part{r: f, size: size})
		return nil
	}
	if err := add(w.path, w.headSize); err != nil {
		return nil, err
	}
	if m := marker(w.dropped); m != "" {
		r.parts = append(r.parts, part{r: strings.NewReader(m), size: int64(len(m))})
	}
	for _, s := range w.segs {
		if err := add(s.path, s.size); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// ToolLine redacts one line of tool output for the log: credentials masked, the
// document values of duplicate-key errors replaced, and the line cut to ToolLineMax.
func ToolLine(line string) string {
	return cut(maskDupKey(redact.Text(line)), ToolLineMax)
}

// MessageLine redacts one of MongoRescue's own lines for the log, cut to
// MessageLineMax.
func MessageLine(line string) string {
	return cut(maskDupKey(redact.Text(line)), MessageLineMax)
}

// dupKeyPattern matches the key of a duplicate key error ("dup key: { email: ... }"),
// which quotes document values.
var dupKeyPattern = regexp.MustCompile(`(?i)(dup key:\s*).*$`)

// maskDupKey removes document values from a duplicate key error.
func maskDupKey(s string) string {
	if !strings.Contains(strings.ToLower(s), "dup key:") {
		return s
	}
	return dupKeyPattern.ReplaceAllString(s, "${1}{ "+redact.Mask+" }")
}

// cut returns s shortened to at most n bytes (on a character boundary) and marked
// with an ellipsis.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}
