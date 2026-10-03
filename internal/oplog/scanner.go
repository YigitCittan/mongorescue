package oplog

import (
	"io"
)

// Scanner is an io.Writer that passes its bytes through to another writer unchanged
// and walks the length-prefixed BSON documents in them, whatever the write
// boundaries. It holds at most one document at a time. Each document must be a valid
// oplog entry with a ts; the Scanner counts them and records the first and last
// (ts, t).
//
// The collector puts a Scanner in the chunk pipeline (reader, Scanner, gzip, age,
// hash, storage); the integrity sweep uses one to walk a stored chunk. Call Finish
// once the stream is complete: it reports a document that was cut short. A Scanner
// is not safe for concurrent use.
type Scanner struct {
	w      io.Writer
	prefix [4]byte
	// prefixN counts the bytes of the current length prefix; doc holds the current
	// document from the length prefix on, filled up to docN.
	prefixN     int
	doc         []byte
	docN        int
	count       int64
	first, last OpTime
	err         error
}

// NewScanner returns a Scanner that writes through to w; a nil w discards the bytes
// (only the walk is wanted).
func NewScanner(w io.Writer) *Scanner {
	if w == nil {
		w = io.Discard
	}
	return &Scanner{w: w}
}

// Write walks p and then writes it to the underlying writer. A malformed document
// stops the Scanner: Write returns ErrMalformed (wrapped) without writing p, and every
// later call returns the same error, as it does after an error of the underlying
// writer.
func (s *Scanner) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if err := s.scan(p); err != nil {
		s.err = err
		return 0, err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// scan consumes p.
func (s *Scanner) scan(p []byte) error {
	for len(p) > 0 {
		if s.prefixN < len(s.prefix) {
			n := copy(s.prefix[s.prefixN:], p)
			s.prefixN += n
			p = p[n:]
			if s.prefixN < len(s.prefix) {
				return nil
			}
			size, err := documentSize(s.prefix[:])
			if err != nil {
				return err
			}
			if cap(s.doc) >= size {
				s.doc = s.doc[:size]
			} else {
				s.doc = make([]byte, size)
			}
			s.docN = copy(s.doc, s.prefix[:])
		}
		n := copy(s.doc[s.docN:], p)
		s.docN += n
		p = p[n:]
		if s.docN < len(s.doc) {
			return nil
		}
		if err := s.entry(s.doc); err != nil {
			return err
		}
		s.prefixN, s.docN = 0, 0
	}
	return nil
}

// entry records one complete document.
func (s *Scanner) entry(doc []byte) error {
	pos, err := entryOpTime(doc)
	if err != nil {
		return err
	}
	if s.count == 0 {
		s.first = pos
	}
	s.last = pos
	s.count++
	return nil
}

// Finish reports whether the stream ended on a document boundary: it returns
// ErrTruncated when a document was cut short, and the error that stopped the Scanner
// if there was one. It does not close the underlying writer.
func (s *Scanner) Finish() error {
	if s.err != nil {
		return s.err
	}
	if s.prefixN > 0 {
		return ErrTruncated
	}
	return nil
}

// Count returns the number of complete entries walked so far.
func (s *Scanner) Count() int64 { return s.count }

// First returns the position of the first entry (the zero OpTime before any).
func (s *Scanner) First() OpTime { return s.first }

// Last returns the position of the last complete entry (the zero OpTime before any).
func (s *Scanner) Last() OpTime { return s.last }
