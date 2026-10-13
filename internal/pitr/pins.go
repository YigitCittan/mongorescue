package pitr

import (
	"log/slog"
	"sync"
	"time"
)

// DefaultPinMaxAge is how long a pin protects what it names after its last
// refresh (see PinSet): a running restore refreshes its pin far more often
// (Pin.KeepAlive), so only a pin its holder abandoned without releasing it
// expires.
const DefaultPinMaxAge = time.Hour

// PinSet holds the pins of the point-in-time restores and chain tests in flight:
// each names the base backup, the archive (storage target and key) and the oplog
// chunks (by chunk ID, which covers the chunk's object and all its copies) that a
// restore planned and still reads. Retention and the purges skip pinned objects
// and delete them on a later pass, once no pin names them. Pinning the archive
// as well as the base ID keeps the old archive of a base that was re-encrypted
// meanwhile (its tombstone record has another ID).
//
// Pins live in memory: a crash ends the restores that hold them too. A pin is
// released by its holder (on every exit path of its restore), which keeps it
// alive while it runs (Pin.KeepAlive). As a safety net, a pin not refreshed for
// its maximum age no longer counts: the next Snapshot drops it, reports it and
// logs a warning. It is safe for concurrent use; a nil *PinSet holds no pins.
type PinSet struct {
	mu     sync.Mutex
	maxAge time.Duration
	now    func() time.Time
	logger *slog.Logger
	seq    uint64
	pins   map[uint64]*pinEntry
}

// PinOptions configures NewPinSet.
type PinOptions struct {
	// MaxAge is how long a pin counts after its last refresh (DefaultPinMaxAge
	// for 0 or less).
	MaxAge time.Duration
	// Now is the clock (time.Now for nil).
	Now func() time.Time
	// Logger receives the warning about an expired pin (slog.Default for nil).
	Logger *slog.Logger
}

// pinEntry is one pin.
type pinEntry struct {
	owner     string
	refreshed time.Time
	what      PinTarget
}

// PinTarget is what one pin names.
type PinTarget struct {
	// Bases are base backup IDs and Chunks oplog chunk IDs.
	Bases  []string
	Chunks []string
	// Archives are the storage objects of the bases the restore reads.
	Archives []ArchiveRef
}

// ArchiveRef names a stored object: a key on a storage target.
type ArchiveRef struct {
	TargetID string
	Key      string
}

// NewPinSet returns an empty pin set.
func NewPinSet(o PinOptions) *PinSet {
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultPinMaxAge
	}
	return &PinSet{maxAge: o.MaxAge, now: o.Now, logger: o.Logger}
}

// sharedPins is the process-wide pin set.
var sharedPins = NewPinSet(PinOptions{})

// SharedPins returns the process-wide pin set, used where none is injected.
func SharedPins() *PinSet { return sharedPins }

// clock returns the current time.
func (p *PinSet) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Pin is one pin of a PinSet. Its methods are safe for concurrent use; a nil
// *Pin does nothing.
type Pin struct {
	set  *PinSet
	id   uint64
	once sync.Once
}

// Pin pins what for owner (a label for logs) and returns the pin; its holder
// must Release it.
func (p *PinSet) Pin(owner string, what PinTarget) *Pin {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pins == nil {
		p.pins = map[uint64]*pinEntry{}
	}
	p.seq++
	what = PinTarget{Bases: append([]string(nil), what.Bases...), Chunks: append([]string(nil), what.Chunks...),
		Archives: append([]ArchiveRef(nil), what.Archives...)}
	p.pins[p.seq] = &pinEntry{owner: owner, refreshed: p.clock(), what: what}
	return &Pin{set: p, id: p.seq}
}

// Release removes the pin; calling it again does nothing.
func (h *Pin) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.set.mu.Lock()
		defer h.set.mu.Unlock()
		delete(h.set.pins, h.id)
	})
}

// Refresh restarts the pin's maximum age. It reports false for a pin that was
// released or already expired.
func (h *Pin) Refresh() bool {
	if h == nil {
		return false
	}
	h.set.mu.Lock()
	defer h.set.mu.Unlock()
	e, ok := h.set.pins[h.id]
	if ok {
		e.refreshed = h.set.clock()
	}
	return ok
}

// KeepAlive refreshes the pin every sixth of its maximum age until stop is
// called; stop waits for the refresher to end.
func (h *Pin) KeepAlive() (stop func()) {
	if h == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(max(h.set.maxAge/6, time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				h.Refresh()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
}

// Len returns how many pins are held (for tests and diagnostics).
func (p *PinSet) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pins)
}

// ExpiredPin is a pin that was not refreshed for its maximum age and no longer
// counts.
type ExpiredPin struct {
	// Owner is the pin's label and Idle how long it went without a refresh.
	Owner string
	Idle  time.Duration
}

// PinSnapshot is what the pins of a PinSet named at one moment.
type PinSnapshot struct {
	bases, chunks map[string]bool
	archives      map[ArchiveRef]bool
	// Expired lists the pins this snapshot dropped for their age.
	Expired []ExpiredPin
}

// Snapshot returns what the current pins name, dropping those not refreshed for
// the maximum age with a warning.
func (p *PinSet) Snapshot() PinSnapshot {
	var out PinSnapshot
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	for id, e := range p.pins {
		if idle := now.Sub(e.refreshed); idle > p.maxAge {
			delete(p.pins, id)
			out.Expired = append(out.Expired, ExpiredPin{Owner: e.owner, Idle: idle})
			continue
		}
		for _, b := range e.what.Bases {
			out.bases = setOf(out.bases, b)
		}
		for _, c := range e.what.Chunks {
			out.chunks = setOf(out.chunks, c)
		}
		for _, a := range e.what.Archives {
			out.archives = setOf(out.archives, a)
		}
	}
	if len(out.Expired) > 0 {
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		for _, e := range out.Expired {
			logger.Warn("dropped a PITR restore pin that was not refreshed: its holder abandoned it; retention may now delete what it held",
				slog.String("owner", e.Owner), slog.Duration("idle", e.Idle))
		}
	}
	return out
}

// setOf adds k to m, creating m when needed.
func setOf[K comparable](m map[K]bool, k K) map[K]bool {
	if m == nil {
		m = map[K]bool{}
	}
	m[k] = true
	return m
}

// Empty reports whether the snapshot pins nothing.
func (s PinSnapshot) Empty() bool {
	return len(s.bases) == 0 && len(s.chunks) == 0 && len(s.archives) == 0
}

// Base reports whether base backup id is pinned.
func (s PinSnapshot) Base(id string) bool { return s.bases[id] }

// Chunk reports whether oplog chunk id (its object and its copies) is pinned.
func (s PinSnapshot) Chunk(id string) bool { return s.chunks[id] }

// Archive reports whether object key on storage target targetID is pinned.
func (s PinSnapshot) Archive(targetID, key string) bool {
	return s.archives[ArchiveRef{TargetID: targetID, Key: key}]
}
