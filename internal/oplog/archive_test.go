package oplog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc64"
	"io"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

// archiveBody is what readArchiveBody finds after the prelude.
type archiveBody struct {
	entries  []bson.Raw
	segments int
}

// readArchiveBody reads the namespace segments and the EOF block that follow an
// archive prelude, as mongorestore's demultiplexer does, and checks the CRC.
func readArchiveBody(t *testing.T, r io.Reader) archiveBody {
	t.Helper()
	var body archiveBody
	crc := crc64.New(crc64.MakeTable(crc64.ECMA))
	readWord := func() uint32 {
		var w [4]byte
		if _, err := io.ReadFull(r, w[:]); err != nil {
			t.Fatalf("read archive: %v", err)
		}
		return binary.LittleEndian.Uint32(w[:])
	}
	readDoc := func(size uint32) bson.Raw {
		doc := make([]byte, size)
		binary.LittleEndian.PutUint32(doc, size)
		if _, err := io.ReadFull(r, doc[4:]); err != nil {
			t.Fatalf("read archive document: %v", err)
		}
		if err := bson.Raw(doc).Validate(); err != nil {
			t.Fatalf("invalid archive document: %v", err)
		}
		return doc
	}
	for {
		header := readDoc(readWord())
		keys, err := header.Elements()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, k := range keys {
			names = append(names, k.Key())
		}
		if len(names) != 4 || names[0] != "db" || names[1] != "collection" || names[2] != "EOF" || names[3] != "CRC" {
			t.Fatalf("namespace header fields = %v, want exactly db, collection, EOF, CRC", names)
		}
		if str(header, "db") != "" || str(header, "collection") != "oplog" {
			t.Fatalf("namespace header for %q.%q, want \"\".oplog", str(header, "db"), str(header, "collection"))
		}
		crcVal := header.Lookup("CRC")
		if crcVal.Type != bson.TypeInt64 {
			t.Fatalf("CRC has type %v, want int64", crcVal.Type)
		}
		if header.Lookup("EOF").Boolean() {
			if got := binary.LittleEndian.Uint64(crcVal.Value); got != crc.Sum64() {
				t.Fatalf("CRC = %x, want %x", got, crc.Sum64())
			}
			if w := readWord(); w != 0xffffffff {
				t.Fatalf("EOF block ends with %x, want the terminator", w)
			}
			if n, _ := r.Read(make([]byte, 1)); n != 0 {
				t.Fatal("bytes after the EOF block")
			}
			return body
		}
		body.segments++
		for {
			w := readWord()
			if w == 0xffffffff {
				break
			}
			doc := readDoc(w)
			_, _ = crc.Write(doc)
			body.entries = append(body.entries, doc)
		}
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	entries := splitEntries(t, readFixture(t, "8.0", "txn"))
	var buf bytes.Buffer
	a, err := NewArchiveWriter(&buf, ArchiveOptions{ServerVersion: "8.0.32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err = a.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if a.Entries() != int64(len(entries)) {
		t.Errorf("entries = %d, want %d", a.Entries(), len(entries))
	}

	r := bytes.NewReader(buf.Bytes())
	prelude, err := mongotools.ReadArchivePrelude(r, 0)
	if err != nil {
		t.Fatalf("read prelude: %v", err)
	}
	if prelude.FormatVersion != "0.1" || prelude.ServerVersion != "8.0.32" || prelude.ToolVersion != DefaultToolVersion {
		t.Errorf("header = %+v", prelude)
	}
	want := []mongotools.ArchiveCollection{{Database: "", Name: "oplog", Type: mongotools.CollectionTypeCollection}}
	if len(prelude.Collections) != 1 || prelude.Collections[0] != want[0] {
		t.Fatalf("collections = %+v, want %+v", prelude.Collections, want)
	}
	body := readArchiveBody(t, r)
	if body.segments != 1 || len(body.entries) != len(entries) {
		t.Fatalf("%d segments, %d entries; want 1 segment, %d entries", body.segments, len(body.entries), len(entries))
	}
	for i := range entries {
		if !bytes.Equal(body.entries[i], entries[i]) {
			t.Fatalf("entry %d changed", i)
		}
	}
}

func TestArchiveEmpty(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchiveWriter(&buf, ArchiveOptions{ServerVersion: "5.0.33", ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(buf.Bytes())
	prelude, err := mongotools.ReadArchivePrelude(r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if prelude.ToolVersion != "test" {
		t.Errorf("tool version = %q", prelude.ToolVersion)
	}
	if body := readArchiveBody(t, r); body.segments != 1 || len(body.entries) != 0 {
		t.Fatalf("empty archive has %d segments, %d entries", body.segments, len(body.entries))
	}
}

func TestArchiveRejects(t *testing.T) {
	a, err := NewArchiveWriter(io.Discard, ArchiveOptions{ServerVersion: "8.0.32"})
	if err != nil {
		t.Fatal(err)
	}
	good := entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "n"}})
	bad := append([]byte{}, good...)
	bad[len(bad)-1] = 1
	for name, e := range map[string][]byte{"short": {5, 0, 0}, "length mismatch": good[:len(good)-1], "no final NUL": bad} {
		if err := a.WriteEntry(e); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteEntry(good); !errors.Is(err, ErrArchiveClosed) {
		t.Errorf("write after close: err = %v, want ErrArchiveClosed", err)
	}

	if _, err := NewArchiveWriter(failWriter{}, ArchiveOptions{ServerVersion: "8.0.32"}); !errors.Is(err, errWrite) {
		t.Errorf("prelude write error: err = %v", err)
	}
	for _, v := range []string{"", "8.0", "8.0.x", "8.0.1.2", "v8.0.1"} {
		if _, err := NewArchiveWriter(io.Discard, ArchiveOptions{ServerVersion: v}); !errors.Is(err, ErrServerVersion) {
			t.Errorf("server version %q: err = %v, want ErrServerVersion", v, err)
		}
	}
	for _, v := range []string{"5.0.33", "8.2.0-rc1", "7.0.1+ent"} {
		if _, err := NewArchiveWriter(io.Discard, ArchiveOptions{ServerVersion: v}); err != nil {
			t.Errorf("server version %q: %v", v, err)
		}
	}
}

// limitWriter fails once n bytes have been written.
type limitWriter struct{ n int }

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errWrite
	}
	w.n -= len(p)
	return len(p), nil
}

func TestArchiveWriteErrorSticks(t *testing.T) {
	var prelude bytes.Buffer
	if _, err := NewArchiveWriter(&prelude, ArchiveOptions{ServerVersion: "8.0.32"}); err != nil {
		t.Fatal(err)
	}
	a, err := NewArchiveWriter(&limitWriter{n: prelude.Len()}, ArchiveOptions{ServerVersion: "8.0.32"})
	if err != nil {
		t.Fatal(err)
	}
	e := entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "n"}})
	if err := a.WriteEntry(e); !errors.Is(err, errWrite) {
		t.Fatalf("write = %v, want the writer's error", err)
	}
	if err := a.WriteEntry(e); !errors.Is(err, errWrite) {
		t.Fatalf("second write = %v, want the same error", err)
	}
	if err := a.Close(); !errors.Is(err, errWrite) {
		t.Fatalf("close = %v, want the writer's error", err)
	}
}
