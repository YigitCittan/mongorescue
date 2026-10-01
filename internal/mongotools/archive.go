package mongotools

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// A mongodump archive (mongodump --archive) starts with the little-endian magic number
// ArchiveMagic and a prelude: a header document, one metadata document per dumped
// namespace and a terminator (int32 -1). The data blocks follow. Every document is
// BSON, prefixed by its int32 length. ReadArchivePrelude reads only the prelude, so the
// collections of a backup can be listed without reading its data.

// ArchiveMagic is the magic number every mongodump archive starts with.
const ArchiveMagic uint32 = 0x8199e26d

// DefaultPreludeLimit bounds the bytes ReadArchivePrelude reads by default.
const DefaultPreludeLimit int64 = 64 << 20

// maxPreludeDocument bounds one prelude document: BSON documents are at most 16 MiB,
// and the archive adds nothing around them. A larger length means the stream is not
// an archive (or is corrupted) rather than a large prelude.
const maxPreludeDocument = 16<<20 + 16<<10

// preludeTerminator ends the prelude (and every data block).
const preludeTerminator = 0xffffffff

// Collection types reported in ArchiveCollection.Type.
const (
	// CollectionTypeCollection is a regular collection.
	CollectionTypeCollection = "collection"
	// CollectionTypeView is a view (no data of its own; see ArchiveCollection.ViewOn).
	CollectionTypeView = "view"
	// CollectionTypeTimeseries is a time-series collection.
	CollectionTypeTimeseries = "timeseries"
)

// Sentinel errors of ReadArchivePrelude.
var (
	// ErrNotArchive indicates that the stream does not start with ArchiveMagic.
	ErrNotArchive = errors.New("mongotools: not a mongodump archive")
	// ErrArchiveTruncated indicates that the stream ended inside the prelude.
	ErrArchiveTruncated = errors.New("mongotools: archive prelude is truncated")
	// ErrPreludeTooLarge indicates that the prelude did not end within the read limit.
	ErrPreludeTooLarge = errors.New("mongotools: archive prelude exceeds the read limit")
	// ErrMalformedPrelude indicates a prelude document that is not valid BSON or lacks
	// its namespace.
	ErrMalformedPrelude = errors.New("mongotools: malformed archive prelude")
)

// ArchivePrelude is the prelude of a mongodump archive.
type ArchivePrelude struct {
	// FormatVersion is the archive format version (e.g. "0.1").
	FormatVersion string
	// ServerVersion is the version of the MongoDB server that was dumped.
	ServerVersion string
	// ToolVersion is the version of mongodump that wrote the archive.
	ToolVersion string
	// Collections lists the dumped namespaces in archive order.
	Collections []ArchiveCollection
}

// ArchiveCollection describes one namespace of an archive prelude.
type ArchiveCollection struct {
	// Database is the database of the namespace.
	Database string
	// Name is the collection (or view) name.
	Name string
	// Type is CollectionTypeCollection, CollectionTypeView or CollectionTypeTimeseries.
	Type string
	// ViewOn is the source collection of a view ("" otherwise).
	ViewOn string
	// SizeBytes is the data size mongodump recorded, 0 when unknown (current mongodump
	// versions do not record it).
	SizeBytes int64
}

// ReadArchivePrelude reads the prelude of the uncompressed mongodump archive in r and
// stops at its terminator: nothing after the prelude is read (r may hold a complete
// archive of any size). At most limit bytes are read (DefaultPreludeLimit when limit
// is not positive); a prelude that does not end within them yields
// ErrPreludeTooLarge. A stream that does not start with ArchiveMagic yields
// ErrNotArchive, one that ends inside the prelude ErrArchiveTruncated and an invalid
// document ErrMalformedPrelude. Read errors of r are returned wrapped.
func ReadArchivePrelude(r io.Reader, limit int64) (*ArchivePrelude, error) {
	if limit <= 0 {
		limit = DefaultPreludeLimit
	}
	pr := &preludeReader{r: r, left: limit}

	var word [4]byte
	if err := pr.readFull(word[:]); err != nil {
		if errors.Is(err, ErrArchiveTruncated) {
			return nil, fmt.Errorf("%w: stream shorter than the magic number", ErrNotArchive)
		}
		return nil, err
	}
	if binary.LittleEndian.Uint32(word[:]) != ArchiveMagic {
		return nil, ErrNotArchive
	}

	prelude := &ArchivePrelude{}
	for first := true; ; first = false {
		if err := pr.readFull(word[:]); err != nil {
			return nil, err
		}
		size := binary.LittleEndian.Uint32(word[:])
		if size == preludeTerminator {
			if first {
				return nil, fmt.Errorf("%w: no header document", ErrMalformedPrelude)
			}
			return prelude, nil
		}
		if size < 5 || size > maxPreludeDocument {
			return nil, fmt.Errorf("%w: document length %d", ErrMalformedPrelude, size)
		}
		if int64(size)-4 > pr.left {
			return nil, ErrPreludeTooLarge
		}
		doc := make([]byte, size)
		copy(doc, word[:])
		if err := pr.readFull(doc[4:]); err != nil {
			return nil, err
		}
		fields, err := bsonScalars(doc)
		if err != nil {
			return nil, err
		}
		if first {
			prelude.FormatVersion = fields.str("version")
			prelude.ServerVersion = fields.str("server_version")
			prelude.ToolVersion = fields.str("tool_version")
			continue
		}
		c, err := collectionFromMetadata(fields)
		if err != nil {
			return nil, err
		}
		prelude.Collections = append(prelude.Collections, c)
	}
}

// collectionFromMetadata builds an ArchiveCollection from a prelude metadata document
// ({db, collection, metadata, size, type}). Archives written before mongodump recorded
// "type" carry the view and time-series options in the metadata JSON only.
func collectionFromMetadata(f bsonFields) (ArchiveCollection, error) {
	c := ArchiveCollection{Database: f.str("db"), Name: f.str("collection"), Type: f.str("type")}
	if c.Name == "" {
		return c, fmt.Errorf("%w: metadata document without a collection name", ErrMalformedPrelude)
	}
	if n, ok := f.num("size"); ok && n > 0 {
		c.SizeBytes = n
	}
	var meta struct {
		Type    string `json:"type"`
		Options struct {
			ViewOn     *string         `json:"viewOn"`
			Timeseries json.RawMessage `json:"timeseries"`
		} `json:"options"`
	}
	// The metadata is mongodump's extended JSON; only its options matter here, so a
	// document that does not parse leaves the type to the "type" field.
	if raw := f.str("metadata"); raw != "" && json.Unmarshal([]byte(raw), &meta) == nil {
		if meta.Options.ViewOn != nil {
			c.ViewOn = *meta.Options.ViewOn
		}
		if c.Type == "" {
			c.Type = meta.Type
		}
		if c.Type == "" && meta.Options.ViewOn != nil {
			c.Type = CollectionTypeView
		}
		if c.Type == "" && len(meta.Options.Timeseries) > 0 && string(meta.Options.Timeseries) != "null" {
			c.Type = CollectionTypeTimeseries
		}
	}
	switch c.Type {
	case CollectionTypeView, CollectionTypeTimeseries:
	default:
		c.Type = CollectionTypeCollection
	}
	return c, nil
}

// preludeReader reads through r, failing with ErrPreludeTooLarge once more than left
// bytes would be read and with ErrArchiveTruncated on a premature end of stream.
type preludeReader struct {
	r    io.Reader
	left int64
}

// readFull fills p.
func (pr *preludeReader) readFull(p []byte) error {
	if int64(len(p)) > pr.left {
		return ErrPreludeTooLarge
	}
	n, err := io.ReadFull(pr.r, p)
	pr.left -= int64(n)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ErrArchiveTruncated
	default:
		return fmt.Errorf("read archive prelude: %w", err)
	}
}

// bsonFields holds the top-level string and numeric fields of a BSON document.
type bsonFields struct {
	strs map[string]string
	nums map[string]int64
}

// str returns string field name ("" when absent).
func (f bsonFields) str(name string) string { return f.strs[name] }

// num returns numeric field name.
func (f bsonFields) num(name string) (int64, bool) {
	n, ok := f.nums[name]
	return n, ok
}

// BSON element types (https://bsonspec.org/spec.html).
const (
	bsonDouble     = 0x01
	bsonString     = 0x02
	bsonDocument   = 0x03
	bsonArray      = 0x04
	bsonBinary     = 0x05
	bsonUndefined  = 0x06
	bsonObjectID   = 0x07
	bsonBool       = 0x08
	bsonDateTime   = 0x09
	bsonNull       = 0x0a
	bsonRegex      = 0x0b
	bsonDBPointer  = 0x0c
	bsonJavaScript = 0x0d
	bsonSymbol     = 0x0e
	bsonCodeScope  = 0x0f
	bsonInt32      = 0x10
	bsonTimestamp  = 0x11
	bsonInt64      = 0x12
	bsonDecimal128 = 0x13
	bsonMinKey     = 0xff
	bsonMaxKey     = 0x7f
)

// bsonScalars decodes the top-level string, int32, int64 and double fields of the
// BSON document doc and skips every other element. It is a minimal reader for the
// archive prelude: the MongoDB driver is not used outside internal/mongoconn.
func bsonScalars(doc []byte) (bsonFields, error) {
	f := bsonFields{strs: map[string]string{}, nums: map[string]int64{}}
	malformed := func(what string) (bsonFields, error) {
		return bsonFields{}, fmt.Errorf("%w: %s", ErrMalformedPrelude, what)
	}
	if len(doc) < 5 || int(binary.LittleEndian.Uint32(doc)) != len(doc) || doc[len(doc)-1] != 0 {
		return malformed("bad document length")
	}
	body := doc[4 : len(doc)-1]
	for len(body) > 0 {
		typ := body[0]
		end := bytes.IndexByte(body[1:], 0)
		if end < 0 {
			return malformed("unterminated element name")
		}
		name := string(body[1 : 1+end])
		body = body[2+end:]
		size, err := bsonValueSize(typ, body)
		if err != nil {
			return bsonFields{}, err
		}
		val := body[:size]
		switch typ {
		case bsonString:
			// int32 length (including the NUL), bytes, NUL.
			f.strs[name] = string(val[4 : len(val)-1])
		case bsonInt32:
			f.nums[name] = int64(int32(binary.LittleEndian.Uint32(val)))
		case bsonInt64:
			f.nums[name] = int64(binary.LittleEndian.Uint64(val))
		case bsonDouble:
			if d := math.Float64frombits(binary.LittleEndian.Uint64(val)); d >= math.MinInt64 && d <= math.MaxInt64 {
				f.nums[name] = int64(d)
			}
		}
		body = body[size:]
	}
	return f, nil
}

// bsonValueSize returns the encoded size of the value of type typ at the start of b.
func bsonValueSize(typ byte, b []byte) (int, error) {
	fixed := func(n int) (int, error) {
		if len(b) < n {
			return 0, fmt.Errorf("%w: truncated element", ErrMalformedPrelude)
		}
		return n, nil
	}
	// prefixed returns the size of a value starting with an int32 length: extra bytes
	// follow the length that it does not count (the binary subtype), and the length
	// counts itself when self is true (documents, arrays, code with scope).
	prefixed := func(extra int, self bool, minLen int) (int, error) {
		if len(b) < 4 {
			return 0, fmt.Errorf("%w: truncated element", ErrMalformedPrelude)
		}
		n := int64(int32(binary.LittleEndian.Uint32(b)))
		if n < int64(minLen) {
			return 0, fmt.Errorf("%w: bad element length", ErrMalformedPrelude)
		}
		total := n + int64(extra)
		if !self {
			total += 4
		}
		if total > int64(len(b)) {
			return 0, fmt.Errorf("%w: truncated element", ErrMalformedPrelude)
		}
		return int(total), nil
	}
	switch typ {
	case bsonDouble, bsonDateTime, bsonTimestamp, bsonInt64:
		return fixed(8)
	case bsonString, bsonJavaScript, bsonSymbol:
		n, err := prefixed(0, false, 1)
		if err == nil && b[n-1] != 0 {
			return 0, fmt.Errorf("%w: unterminated string", ErrMalformedPrelude)
		}
		return n, err
	case bsonDocument, bsonArray:
		return prefixed(0, true, 5)
	case bsonCodeScope:
		return prefixed(0, true, 14)
	case bsonBinary:
		return prefixed(1, false, 0)
	case bsonUndefined, bsonNull, bsonMinKey, bsonMaxKey:
		return 0, nil
	case bsonObjectID:
		return fixed(12)
	case bsonBool:
		return fixed(1)
	case bsonInt32:
		return fixed(4)
	case bsonDecimal128:
		return fixed(16)
	case bsonRegex:
		// Two C strings: pattern and options.
		first := bytes.IndexByte(b, 0)
		if first < 0 {
			return 0, fmt.Errorf("%w: unterminated regex", ErrMalformedPrelude)
		}
		second := bytes.IndexByte(b[first+1:], 0)
		if second < 0 {
			return 0, fmt.Errorf("%w: unterminated regex", ErrMalformedPrelude)
		}
		return first + second + 2, nil
	case bsonDBPointer:
		n, err := prefixed(0, false, 1)
		if err != nil {
			return 0, err
		}
		if n+12 > len(b) {
			return 0, fmt.Errorf("%w: truncated element", ErrMalformedPrelude)
		}
		return n + 12, nil
	default:
		return 0, fmt.Errorf("%w: unknown element type 0x%02x", ErrMalformedPrelude, typ)
	}
}
