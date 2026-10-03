package oplog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MaxEntrySize bounds one oplog entry: a 16 MiB document plus the 16 KiB of oplog
// fields MongoDB allows around it. A longer length prefix means the stream is corrupt.
const MaxEntrySize = 16<<20 + 16<<10

// minDocumentSize is the size of an empty BSON document.
const minDocumentSize = 5

// Sentinel errors.
var (
	// ErrTruncated indicates that a stream ended inside a BSON document.
	ErrTruncated = errors.New("oplog: truncated BSON document")
	// ErrMalformed indicates a document that is not valid BSON or not a valid oplog
	// entry (a length prefix out of range, a missing ts, op or ns).
	ErrMalformed = errors.New("oplog: malformed oplog entry")
	// ErrUnknownOp indicates an entry whose operation type the Filter does not know.
	ErrUnknownOp = errors.New("oplog: unknown operation type")
	// ErrUnknownCommand indicates a command entry the Filter does not know how to
	// rewrite. It is refused rather than replayed unchanged.
	ErrUnknownCommand = errors.New("oplog: unknown command entry")
	// ErrCrossSelectionRename indicates a rename between a selected and an unselected
	// database, which a filtered replay cannot reproduce.
	ErrCrossSelectionRename = errors.New("oplog: rename across the database selection")
	// ErrNoTarget indicates a Filter with neither Rename nor InPlace, or both.
	ErrNoTarget = errors.New("oplog: filter needs either Rename or InPlace")
	// ErrBadRename indicates that Filter.Rename returned an invalid database name or
	// the source name itself.
	ErrBadRename = errors.New("oplog: invalid renamed database name")
	// ErrServerVersion indicates an ArchiveOptions.ServerVersion that mongorestore
	// cannot parse.
	ErrServerVersion = errors.New("oplog: invalid server version for the archive")
	// ErrArchiveClosed indicates a write to a closed ArchiveWriter.
	ErrArchiveClosed = errors.New("oplog: archive writer is closed")
)

// OpTime is the position of an oplog entry: its timestamp and election term.
type OpTime struct {
	// TS is the entry's ts.
	TS bson.Timestamp
	// T is the entry's term t, -1 when the entry has none.
	T int64
}

// Before reports whether a sorts before b by timestamp.
func Before(a, b bson.Timestamp) bool {
	return a.T < b.T || (a.T == b.T && a.I < b.I)
}

// documentSize decodes and checks the length prefix of a BSON document.
func documentSize(prefix []byte) (int, error) {
	n := binary.LittleEndian.Uint32(prefix)
	if n < minDocumentSize || n > MaxEntrySize {
		return 0, fmt.Errorf("%w: document length %d", ErrMalformed, n)
	}
	return int(n), nil
}

// entryOpTime validates doc and returns its (ts, t).
func entryOpTime(doc bson.Raw) (OpTime, error) {
	if err := doc.Validate(); err != nil {
		return OpTime{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	t, i, ok := doc.Lookup("ts").TimestampOK()
	if !ok {
		return OpTime{}, fmt.Errorf("%w: no timestamp field ts", ErrMalformed)
	}
	pos := OpTime{TS: bson.Timestamp{T: t, I: i}, T: -1}
	if term, ok := doc.Lookup("t").AsInt64OK(); ok {
		pos.T = term
	}
	return pos, nil
}

// Reader reads the oplog entries of a stream of concatenated BSON documents, such as
// a decrypted and decompressed chunk, one at a time.
type Reader struct {
	r io.Reader
}

// NewReader returns a Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Next returns the next document. It returns io.EOF at the end of the stream,
// ErrTruncated when the stream ends inside a document and ErrMalformed for a length
// prefix out of range. Read errors of the underlying reader are returned wrapped. The
// document is only checked to be complete; Filter and Scanner validate its contents.
func (r *Reader) Next() (bson.Raw, error) {
	var prefix [4]byte
	n, err := io.ReadFull(r.r, prefix[:])
	switch {
	case err == nil:
	case errors.Is(err, io.EOF) && n == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return nil, ErrTruncated
	default:
		return nil, fmt.Errorf("read oplog entry: %w", err)
	}
	size, err := documentSize(prefix[:])
	if err != nil {
		return nil, err
	}
	doc := make([]byte, size)
	copy(doc, prefix[:])
	if _, err := io.ReadFull(r.r, doc[4:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrTruncated
		}
		return nil, fmt.Errorf("read oplog entry: %w", err)
	}
	return doc, nil
}
