package pitr

import (
	"sync"
	"time"
)

// DefaultPinMaxAge is how long a pin protects what it names at most (see PinSet):
// far longer than any restore, and short enough that a pin a bug never released
// cannot hold retention back for good.
const DefaultPinMaxAge = 72 * time.Hour

// PinSet holds the pins of the point-in-time restores and chain tests in flight:
// each names the base backup and the oplog chunks (by chunk ID, which covers the
// chunk's object and all its copies) that a restore planned and still reads.
// Retention and the purges skip pinned objects and delete them on a later pass,
// once no pin names them.
//
// Pins live in memory: a crash ends the restores that hold them too. A pin is
// released by the function Pin returns (on every exit path of its restore); as a
// safety net, one older than its maximum age no longer counts and is dropped by
// the next Snapshot, which reports it. It is safe for concurrent use; the zero
// value and a nil *PinSet hold no pins.
type PinSet struct {
	mu     sync.Mutex
	maxAge time.Duration
	now    func() time.Time
	seq    uint64
	pins   map[uint64]*pinEntry
}

// pinEntry is one pin.
type pinEntry struct {
	owner  string
	at     time.Time
	bases  []string
	chunks []string
}

// NewPinSet returns an empty pin set whose pins count for maxAge at most
// (DefaultPinMaxAge for 0 or less) by the clock now (time.Now for nil).
func NewPinSet(maxAge time.Duration, now func() time.Time) *PinSet {
	if maxAge <= 0 {
		maxAge = DefaultPinMaxAge
	}
	return &PinSet{maxAge: maxAge, now: now}
}

// sharedPins is the process-wide pin set.
var sharedPins = NewPinSet(DefaultPinMaxAge, nil)

// SharedPins returns the process-wide pin set, which the restores and the
// retention and purges of one process share unless a test injects its own.
func SharedPins() *PinSet { return sharedPins }

// clock returns the current time.
func (p *PinSet) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Pin pins base backups bases and oplog chunks chunks for owner (a label for
// logs) and returns the release function; calling it again does nothing.
func (p *PinSet) Pin(owner string, bases, chunks []string) (release func()) {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pins == nil {
		p.pins = map[uint64]*pinEntry{}
	}
	p.seq++
	id := p.seq
	p.pins[id] = &pinEntry{owner: owner, at: p.clock(), bases: append([]string(nil), bases...),
		chunks: append([]string(nil), chunks...)}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			delete(p.pins, id)
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

// ExpiredPin is a pin that outlived its maximum age and no longer counts.
type ExpiredPin struct {
	// Owner is the pin's label and Age how old it was when it was dropped.
	Owner string
	Age   time.Duration
}

// PinSnapshot is what the pins of a PinSet named at one moment.
type PinSnapshot struct {
	bases, chunks map[string]bool
	// Expired lists the pins this snapshot dropped for their age.
	Expired []ExpiredPin
}

// Snapshot returns what the current pins name, dropping (and reporting) those
// older than the maximum age.
func (p *PinSet) Snapshot() PinSnapshot {
	var out PinSnapshot
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	maxAge := p.maxAge
	if maxAge <= 0 {
		maxAge = DefaultPinMaxAge
	}
	for id, e := range p.pins {
		if age := now.Sub(e.at); age > maxAge {
			delete(p.pins, id)
			out.Expired = append(out.Expired, ExpiredPin{Owner: e.owner, Age: age})
			continue
		}
		for _, b := range e.bases {
			if out.bases == nil {
				out.bases = map[string]bool{}
			}
			out.bases[b] = true
		}
		for _, c := range e.chunks {
			if out.chunks == nil {
				out.chunks = map[string]bool{}
			}
			out.chunks[c] = true
		}
	}
	return out
}

// Empty reports whether the snapshot pins nothing.
func (s PinSnapshot) Empty() bool { return len(s.bases) == 0 && len(s.chunks) == 0 }

// Base reports whether base backup id is pinned.
func (s PinSnapshot) Base(id string) bool { return s.bases[id] }

// Chunk reports whether oplog chunk id (its object and its copies) is pinned.
func (s PinSnapshot) Chunk(id string) bool { return s.chunks[id] }
