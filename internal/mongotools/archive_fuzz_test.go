package mongotools

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fuzzPreludeLimit is the read limit of FuzzReadPrelude.
const fuzzPreludeLimit = 4096

// fuzzAllocSlack covers allocations of the runtime and the fuzzing engine itself
// that happen while a call is measured.
const fuzzAllocSlack = 256 << 10

// deadlineReader serves the first cut bytes of r, then fails every read with
// context.DeadlineExceeded, as the engine's context-bound reader does once the
// preview times out. It records reads attempted after the first failure.
type deadlineReader struct {
	r          io.Reader
	cut        int
	n          int
	failed     bool
	afterError int
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.failed {
		d.afterError++
	}
	if d.n >= d.cut {
		d.failed = true
		return 0, context.DeadlineExceeded
	}
	if len(p) > d.cut-d.n {
		p = p[:d.cut-d.n]
	}
	n, err := d.r.Read(p)
	d.n += n
	return n, err
}

// FuzzReadPrelude checks the prelude reader on arbitrary input, optionally cut off by
// a timeout after cut bytes: it never panics, never reads past the limit, never
// allocates more than a small multiple of the limit (whatever document lengths the
// input declares), stops at the first read error without reading again, and fails
// only with its sentinels or the read error.
func FuzzReadPrelude(f *testing.F) {
	plain, err := os.ReadFile(filepath.Join("testdata", "prelude", "shop.archive"))
	if err != nil {
		f.Fatal(err)
	}
	gz, err := os.ReadFile(filepath.Join("testdata", "prelude", "shop.archive.gz"))
	if err != nil {
		f.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		f.Fatal(err)
	}
	unzipped, err := io.ReadAll(zr)
	if err != nil {
		f.Fatal(err)
	}
	const noCut = 0xffff
	for _, seed := range [][]byte{plain, unzipped} {
		f.Add(seed, uint16(noCut))
		for _, cut := range []int{0, 3, 4, 8, 0x6c, 0x100, 0x200, len(seed) / 2} {
			f.Add(seed[:cut], uint16(noCut)) // truncated stream
			f.Add(seed, uint16(cut))         // timed out after cut bytes
		}
	}
	f.Add([]byte{0x6d, 0xe2, 0x99, 0x81, 0xff, 0xff, 0xff, 0xff}, uint16(noCut))
	f.Add([]byte{0x6d, 0xe2, 0x99, 0x81, 0x00, 0x00, 0x00, 0x01}, uint16(noCut)) // 16 MiB declared
	f.Add([]byte("not an archive"), uint16(noCut))

	f.Fuzz(func(t *testing.T, in []byte, cut uint16) {
		counted := &countingReader{r: bytes.NewReader(in)}
		dr := &deadlineReader{r: counted, cut: int(cut)}
		if cut == noCut {
			dr.cut = len(in) + 1
		}

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		p, err := ReadArchivePrelude(dr, fuzzPreludeLimit)
		runtime.ReadMemStats(&after)

		if counted.n > fuzzPreludeLimit {
			t.Fatalf("read %d bytes past the %d byte limit", counted.n, fuzzPreludeLimit)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 3*fuzzPreludeLimit+fuzzAllocSlack {
			t.Fatalf("allocated %d bytes for a %d byte limit", alloc, fuzzPreludeLimit)
		}
		if dr.afterError > 0 {
			t.Fatalf("read %d more times after the read failed", dr.afterError)
		}
		if err != nil {
			if dr.failed && !errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, ErrPreludeTooLarge) && !errors.Is(err, ErrMalformedPrelude) && !errors.Is(err, ErrNotArchive) {
				t.Fatalf("timed-out read: %v; want the deadline error", err)
			}
			for _, want := range []error{ErrNotArchive, ErrArchiveTruncated, ErrPreludeTooLarge, ErrMalformedPrelude, context.DeadlineExceeded} {
				if errors.Is(err, want) {
					return
				}
			}
			t.Fatalf("unexpected error %v", err)
		}
		if dr.failed {
			t.Fatal("the prelude was reported complete although the stream failed before its end")
		}
		for _, c := range p.Collections {
			if c.Name == "" || (c.Type != CollectionTypeCollection && c.Type != CollectionTypeView && c.Type != CollectionTypeTimeseries) {
				t.Fatalf("invalid collection %+v", c)
			}
		}
	})
}
