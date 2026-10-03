package oplog

import (
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

// The synthetic archive follows the mongodump archive format (mongo-tools
// common/archive/spec.md; internal/mongotools reads its prelude):
//
//	magic (mongotools.ArchiveMagic, little-endian)
//	header {concurrent_collections, version, server_version, tool_version}
//	collection metadata {db: "", collection: "oplog", metadata: "", size, type}
//	terminator (0xffffffff)
//	namespace header {db: "", collection: "oplog", EOF: false, CRC: 0}
//	entries ...
//	terminator
//	EOF header {db: "", collection: "oplog", EOF: true, CRC: <CRC-64-ECMA of the entries>}
//	terminator
//
// mongorestore checks the CRC when it reads the EOF header. Database "" and
// collection "oplog" is where mongodump --oplog puts the oplog, and where
// mongorestore --oplogReplay looks for it (S2 in docs/design/pitr.md).

const (
	// archiveFormatVersion is the only archive format version.
	archiveFormatVersion = "0.1"
	// oplogCollection is the collection of the oplog namespace (database "").
	oplogCollection = "oplog"
	// DefaultToolVersion is the tool_version an ArchiveWriter records by default.
	DefaultToolVersion = "mongorescue"
)

// terminator ends the prelude and every namespace segment.
var terminator = []byte{0xff, 0xff, 0xff, 0xff}

// crcTable is the table of the archive's per-namespace CRC.
var crcTable = crc64.MakeTable(crc64.ECMA)

// ArchiveOptions configures an ArchiveWriter.
type ArchiveOptions struct {
	// ServerVersion is the MongoDB version the entries come from ("8.0.32"); it is
	// required, as mongorestore refuses an archive without a valid one, and warns
	// when its minor version differs from the target's.
	ServerVersion string
	// ToolVersion is recorded as the archive's tool_version (DefaultToolVersion when
	// empty).
	ToolVersion string
}

// ArchiveWriter writes a mongodump archive whose only namespace is the oplog
// (database "", collection "oplog"), for mongorestore --archive --oplogReplay. It
// streams: each entry is written as it comes, and only the running CRC is kept.
// Close ends the archive. An ArchiveWriter is not safe for concurrent use.
type ArchiveWriter struct {
	w       io.Writer
	crc     hash.Hash64
	open    bool // a namespace segment has been started
	closed  bool
	entries int64
	err     error
}

// NewArchiveWriter writes the archive's magic number and prelude to w and returns a
// writer for its entries. An opts.ServerVersion mongorestore cannot parse yields
// ErrServerVersion.
func NewArchiveWriter(w io.Writer, opts ArchiveOptions) (*ArchiveWriter, error) {
	if !validServerVersion(opts.ServerVersion) {
		return nil, fmt.Errorf("%w: %q", ErrServerVersion, opts.ServerVersion)
	}
	if opts.ToolVersion == "" {
		opts.ToolVersion = DefaultToolVersion
	}
	header, err := bson.Marshal(bson.D{
		{Key: "concurrent_collections", Value: int32(1)},
		{Key: "version", Value: archiveFormatVersion},
		{Key: "server_version", Value: opts.ServerVersion},
		{Key: "tool_version", Value: opts.ToolVersion},
	})
	if err != nil {
		return nil, fmt.Errorf("encode archive header: %w", err)
	}
	metadata, err := bson.Marshal(bson.D{
		{Key: "db", Value: ""},
		{Key: "collection", Value: oplogCollection},
		{Key: "metadata", Value: ""},
		{Key: "size", Value: int32(0)},
		{Key: "type", Value: ""},
	})
	if err != nil {
		return nil, fmt.Errorf("encode archive metadata: %w", err)
	}
	a := &ArchiveWriter{w: w, crc: crc64.New(crcTable)}
	magic := binary.LittleEndian.AppendUint32(nil, mongotools.ArchiveMagic)
	for _, b := range [][]byte{magic, header, metadata, terminator} {
		if err := a.write(b); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// WriteEntry appends one oplog entry, a complete BSON document. Only its framing is
// checked (the length prefix and the final NUL); Filter validates the contents.
func (a *ArchiveWriter) WriteEntry(entry []byte) error {
	if a.closed {
		return ErrArchiveClosed
	}
	if a.err != nil {
		return a.err
	}
	if len(entry) < minDocumentSize || len(entry) > MaxEntrySize ||
		int(binary.LittleEndian.Uint32(entry)) != len(entry) || entry[len(entry)-1] != 0 {
		return fmt.Errorf("%w: entry is not a complete BSON document", ErrMalformed)
	}
	if err := a.openSegment(); err != nil {
		return err
	}
	if err := a.write(entry); err != nil {
		return err
	}
	_, _ = a.crc.Write(entry) // a hash never fails
	a.entries++
	return nil
}

// Entries returns the number of entries written.
func (a *ArchiveWriter) Entries() int64 { return a.entries }

// Close ends the archive with the oplog namespace's EOF header and CRC. It does not
// close the underlying writer. Calling Close again does nothing.
func (a *ArchiveWriter) Close() error {
	if a.closed {
		return a.err
	}
	a.closed = true
	if a.err != nil {
		return a.err
	}
	// An archive without entries still gets an (empty) segment: mongorestore fails
	// with "archive io error" when the oplog's EOF header is its first block.
	if err := a.openSegment(); err != nil {
		return err
	}
	if err := a.write(terminator); err != nil {
		return err
	}
	eof, err := namespaceHeader(true, a.crc.Sum64())
	if err != nil {
		return err
	}
	if err := a.write(eof); err != nil {
		return err
	}
	return a.write(terminator)
}

// openSegment writes the namespace header of the segment unless it was written.
func (a *ArchiveWriter) openSegment() error {
	if a.open {
		return nil
	}
	header, err := namespaceHeader(false, 0)
	if err != nil {
		return err
	}
	if err := a.write(header); err != nil {
		return err
	}
	a.open = true
	return nil
}

// validServerVersion reports whether mongorestore can parse v: three dot-separated
// integers, optionally followed by a "-" or "+" build suffix.
func validServerVersion(v string) bool {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// write writes b to the underlying writer and remembers the first error.
func (a *ArchiveWriter) write(b []byte) error {
	if _, err := a.w.Write(b); err != nil {
		a.err = fmt.Errorf("write oplog archive: %w", err)
		return a.err
	}
	return nil
}

// namespaceHeader encodes the namespace header of the oplog namespace. The CRC is a
// uint64 stored in an int64 field; its bits are written as they are.
func namespaceHeader(eof bool, crc uint64) ([]byte, error) {
	b, err := bson.Marshal(bson.D{
		{Key: "db", Value: ""},
		{Key: "collection", Value: oplogCollection},
		{Key: "EOF", Value: eof},
		{Key: "CRC", Value: bson.RawValue{Type: bson.TypeInt64, Value: binary.LittleEndian.AppendUint64(nil, crc)}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode archive namespace header: %w", err)
	}
	return b, nil
}
