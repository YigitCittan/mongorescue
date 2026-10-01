package mongotools

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// testdata/prelude holds real archives written by mongodump 100.16.0 from MongoDB
// 8.0 (database shop: collections orders and customers, the view big_orders on
// orders and the time-series collection metrics), plain and with --gzip.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "prelude", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantShop is the prelude of the shop fixtures, in archive order.
var wantShop = []ArchiveCollection{
	{Database: "shop", Name: "metrics", Type: CollectionTypeTimeseries},
	{Database: "shop", Name: "customers", Type: CollectionTypeCollection},
	{Database: "shop", Name: "orders", Type: CollectionTypeCollection},
	{Database: "shop", Name: "big_orders", Type: CollectionTypeView, ViewOn: "orders"},
}

func checkCollections(t *testing.T, got, want []ArchiveCollection) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("collections = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collection %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadArchivePreludeOfRealArchive(t *testing.T) {
	archive := readFixture(t, "shop.archive")
	cr := &countingReader{r: bytes.NewReader(archive)}
	p, err := ReadArchivePrelude(cr, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.FormatVersion != "0.1" || p.ServerVersion != "8.0.32" || p.ToolVersion != "100.16.0" {
		t.Fatalf("header = %+v", p)
	}
	checkCollections(t, p.Collections, wantShop)

	// Reading stops at the terminator: the next bytes are the first data block, whose
	// namespace header carries an EOF flag.
	if cr.n >= len(archive) {
		t.Fatalf("read %d of %d bytes; the data blocks must not be read", cr.n, len(archive))
	}
	if !bytes.Equal(archive[cr.n-4:cr.n], []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("stopped at byte %d, not after the prelude terminator", cr.n)
	}
	if next := archive[cr.n:min(cr.n+80, len(archive))]; !bytes.Contains(next, []byte("EOF")) {
		t.Fatalf("bytes after the prelude are not a data block header: %q", next)
	}
}

func TestReadArchivePreludeOfRealGzipArchive(t *testing.T) {
	zr, err := gzip.NewReader(bytes.NewReader(readFixture(t, "shop.archive.gz")))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadArchivePrelude(zr, 0)
	if err != nil {
		t.Fatal(err)
	}
	checkCollections(t, p.Collections, wantShop)
}

// archiveDoc marshals v as a BSON document for buildArchive.
func archiveDoc(t *testing.T, v any) []byte {
	t.Helper()
	b, err := bson.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildArchive returns a mongodump archive with the header and metadata documents
// docs, the terminator and data.
func buildArchive(t *testing.T, docs [][]byte, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, ArchiveMagic)
	b.Write(archiveDoc(t, bson.D{{Key: "concurrent_collections", Value: int32(4)}, {Key: "version", Value: "0.1"},
		{Key: "server_version", Value: "4.0.28"}, {Key: "tool_version", Value: "r4.0.28"}}))
	for _, d := range docs {
		b.Write(d)
	}
	b.Write([]byte{0xff, 0xff, 0xff, 0xff})
	b.Write(data)
	return b.Bytes()
}

// TestReadArchivePreludeWithoutTypeField covers archives of older mongodump versions,
// whose metadata documents have no "type": views and time-series collections are
// recognised from their options. Unknown fields of every BSON type are skipped.
func TestReadArchivePreludeWithoutTypeField(t *testing.T) {
	docs := [][]byte{
		archiveDoc(t, bson.D{{Key: "db", Value: "crm"}, {Key: "collection", Value: "people"},
			{Key: "metadata", Value: `{"options":{},"indexes":[]}`}, {Key: "size", Value: int64(4096)},
			{Key: "oid", Value: bson.NewObjectID()}, {Key: "flag", Value: true}, {Key: "nested", Value: bson.D{{Key: "a", Value: 1}}},
			{Key: "list", Value: bson.A{1, "x"}}, {Key: "bin", Value: bson.Binary{Subtype: 4, Data: []byte("0123456789abcdef")}},
			{Key: "re", Value: bson.Regex{Pattern: "^a", Options: "i"}}, {Key: "null", Value: nil}, {Key: "d", Value: 1.5},
			{Key: "dec", Value: bson.NewDecimal128(1, 2)}, {Key: "ts", Value: bson.Timestamp{T: 1, I: 2}},
			{Key: "dt", Value: bson.DateTime(1)}, {Key: "js", Value: bson.JavaScript("x")}, {Key: "min", Value: bson.MinKey{}},
			{Key: "max", Value: bson.MaxKey{}}}),
		archiveDoc(t, bson.D{{Key: "db", Value: "crm"}, {Key: "collection", Value: "adults"},
			{Key: "metadata", Value: `{"options":{"viewOn":"people","pipeline":[]},"indexes":[]}`}, {Key: "size", Value: int32(0)}}),
		archiveDoc(t, bson.D{{Key: "db", Value: "crm"}, {Key: "collection", Value: "ticks"},
			{Key: "metadata", Value: `{"options":{"timeseries":{"timeField":"t"}}}`}, {Key: "size", Value: 12.0}}),
		archiveDoc(t, bson.D{{Key: "db", Value: "crm"}, {Key: "collection", Value: "odd"}, {Key: "metadata", Value: "not json"}}),
	}
	p, err := ReadArchivePrelude(bytes.NewReader(buildArchive(t, docs, []byte("data"))), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ServerVersion != "4.0.28" {
		t.Fatalf("header = %+v", p)
	}
	checkCollections(t, p.Collections, []ArchiveCollection{
		{Database: "crm", Name: "people", Type: CollectionTypeCollection, SizeBytes: 4096},
		{Database: "crm", Name: "adults", Type: CollectionTypeView, ViewOn: "people"},
		{Database: "crm", Name: "ticks", Type: CollectionTypeTimeseries, SizeBytes: 12},
		{Database: "crm", Name: "odd", Type: CollectionTypeCollection},
	})
}

func TestReadArchivePreludeEmptyDatabase(t *testing.T) {
	p, err := ReadArchivePrelude(bytes.NewReader(buildArchive(t, nil, nil)), 0)
	if err != nil || len(p.Collections) != 0 {
		t.Fatalf("prelude = %+v, %v; want no collections", p, err)
	}
}

func TestReadArchivePreludeTruncated(t *testing.T) {
	archive := readFixture(t, "shop.archive")
	cr := &countingReader{r: bytes.NewReader(archive)}
	if _, err := ReadArchivePrelude(cr, 0); err != nil {
		t.Fatal(err)
	}
	preludeEnd := cr.n
	for _, cut := range []int{0, 2} {
		if _, err := ReadArchivePrelude(bytes.NewReader(archive[:cut]), 0); !errors.Is(err, ErrNotArchive) {
			t.Fatalf("archive cut at %d: %v; want ErrNotArchive", cut, err)
		}
	}
	for _, cut := range []int{4, 6, 50, 0x6c, 0x100, preludeEnd - 3, preludeEnd - 1} {
		if _, err := ReadArchivePrelude(bytes.NewReader(archive[:cut]), 0); !errors.Is(err, ErrArchiveTruncated) {
			t.Fatalf("archive cut at %d: %v; want ErrArchiveTruncated", cut, err)
		}
	}
	// A gzip stream cut inside the prelude.
	gz := readFixture(t, "shop.archive.gz")
	zr, err := gzip.NewReader(bytes.NewReader(gz[:len(gz)/3]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReadArchivePrelude(zr, 0); !errors.Is(err, ErrArchiveTruncated) {
		t.Fatalf("truncated gzip archive: %v; want ErrArchiveTruncated", err)
	}
}

func TestReadArchivePreludeLimit(t *testing.T) {
	archive := readFixture(t, "shop.archive")
	for _, limit := range []int64{2, 100, 600} {
		if _, err := ReadArchivePrelude(bytes.NewReader(archive), limit); !errors.Is(err, ErrPreludeTooLarge) {
			t.Fatalf("limit %d: %v; want ErrPreludeTooLarge", limit, err)
		}
	}
	// A huge metadata document is refused before it is allocated or read.
	big := archiveDoc(t, bson.D{{Key: "db", Value: "x"}, {Key: "collection", Value: "y"}, {Key: "metadata", Value: strings.Repeat("m", 1<<20)}})
	cr := &countingReader{r: bytes.NewReader(buildArchive(t, [][]byte{big}, nil))}
	if _, err := ReadArchivePrelude(cr, 64<<10); !errors.Is(err, ErrPreludeTooLarge) {
		t.Fatalf("1 MiB document under a 64 KiB limit: %v; want ErrPreludeTooLarge", err)
	}
	if cr.n > 64<<10 {
		t.Fatalf("read %d bytes past a 64 KiB limit", cr.n)
	}
	// A prelude that never ends is cut at the limit.
	header := buildArchive(t, nil, nil)
	endless := io.MultiReader(bytes.NewReader(header[:len(header)-4]), &repeatReader{doc: archiveDoc(t, bson.D{{Key: "db", Value: "x"}, {Key: "collection", Value: "y"}})})
	if _, err := ReadArchivePrelude(endless, 1<<20); !errors.Is(err, ErrPreludeTooLarge) {
		t.Fatalf("endless prelude: %v; want ErrPreludeTooLarge", err)
	}
}

// repeatReader repeats doc forever.
type repeatReader struct {
	doc []byte
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.doc[r.off:])
		n += c
		r.off = (r.off + c) % len(r.doc)
	}
	return n, nil
}

func TestReadArchivePreludeMalformed(t *testing.T) {
	header := buildArchive(t, nil, nil)
	header = header[:len(header)-4] // magic and header document, no terminator
	cases := map[string][]byte{
		"fake archive":       append([]byte{0x6d, 0xe2, 0x99, 0x81}, []byte("\x00fake mongodump archive; TEST ONLY")...),
		"tiny length":        append(append([]byte{}, header...), 4, 0, 0, 0),
		"huge length":        append(append([]byte{}, header...), 0, 0, 0, 0x7f),
		"no header":          {0x6d, 0xe2, 0x99, 0x81, 0xff, 0xff, 0xff, 0xff},
		"no collection name": buildArchive(t, [][]byte{archiveDoc(t, bson.D{{Key: "db", Value: "x"}})}, nil),
		"bad element type":   append(append([]byte{}, header...), 8, 0, 0, 0, 0x42, 'a', 0, 0),
		"bad string length":  append(append([]byte{}, header...), 12, 0, 0, 0, 0x02, 'a', 0, 0x40, 0, 0, 0, 0),
		"no document end":    append(append([]byte{}, header...), 6, 0, 0, 0, 0x0a, 'a'),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadArchivePrelude(bytes.NewReader(in), 0)
			if !errors.Is(err, ErrMalformedPrelude) && !errors.Is(err, ErrArchiveTruncated) {
				t.Fatalf("err = %v; want ErrMalformedPrelude", err)
			}
		})
	}
	if _, err := ReadArchivePrelude(strings.NewReader("PK\x03\x04 not an archive"), 0); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("zip input: %v; want ErrNotArchive", err)
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestReadArchivePreludeReadError(t *testing.T) {
	boom := errors.New("boom")
	in := io.MultiReader(bytes.NewReader(readFixture(t, "shop.archive")[:200]), failingReader{boom})
	if _, err := ReadArchivePrelude(in, 0); !errors.Is(err, boom) {
		t.Fatalf("err = %v; want the read error", err)
	}
}

// FuzzReadArchivePrelude checks that arbitrary input never panics, never reads past
// the limit and fails only with the documented sentinels.
func FuzzReadArchivePrelude(f *testing.F) {
	archive, err := os.ReadFile(filepath.Join("testdata", "prelude", "shop.archive"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(archive)
	f.Add(archive[:300])
	f.Add([]byte{0x6d, 0xe2, 0x99, 0x81, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte("not an archive"))
	const limit = 4096
	f.Fuzz(func(t *testing.T, in []byte) {
		cr := &countingReader{r: bytes.NewReader(in)}
		p, err := ReadArchivePrelude(cr, limit)
		if cr.n > limit {
			t.Fatalf("read %d bytes past the %d byte limit", cr.n, limit)
		}
		if err != nil {
			for _, sentinel := range []error{ErrNotArchive, ErrArchiveTruncated, ErrPreludeTooLarge, ErrMalformedPrelude} {
				if errors.Is(err, sentinel) {
					return
				}
			}
			t.Fatalf("unexpected error %v", err)
		}
		for _, c := range p.Collections {
			if c.Name == "" || (c.Type != CollectionTypeCollection && c.Type != CollectionTypeView && c.Type != CollectionTypeTimeseries) {
				t.Fatalf("invalid collection %+v", c)
			}
		}
	})
}
