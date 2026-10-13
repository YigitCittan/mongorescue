package oplog

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

// FuzzArchiveWriter checks the synthetic oplog-only archive on arbitrary entries and
// versions: NewArchiveWriter refuses only a server version mongorestore cannot parse,
// WriteEntry takes exactly the complete BSON documents, and what it writes reads back
// through mongotools.ReadArchivePrelude and the namespace segments with the same
// versions, the same entries in order and a matching CRC.
func FuzzArchiveWriter(f *testing.F) {
	fuzzSeeds(f, func(data []byte) { f.Add(data, "8.0.32", "") })
	f.Add([]byte{}, "5.0.33", "test")
	f.Add([]byte{5, 0, 0, 0, 0}, "8.2.0-rc1", "tool\x00version")
	f.Add([]byte{5, 0, 0, 0, 1}, "7.0.1+ent", "")
	f.Add([]byte{}, "8.0", "")
	f.Fuzz(func(t *testing.T, data []byte, serverVersion, toolVersion string) {
		var buf bytes.Buffer
		a, err := NewArchiveWriter(&buf, ArchiveOptions{ServerVersion: serverVersion, ToolVersion: toolVersion})
		if err != nil {
			if !errors.Is(err, ErrServerVersion) {
				t.Fatalf("NewArchiveWriter: %v", err)
			}
			return
		}
		// Write the documents at the start of data, then the rest as one entry. Valid
		// BSON must be taken; anything else goes to a second writer, which may take a
		// complete frame (WriteEntry checks only the framing) or refuse it.
		side, err := NewArchiveWriter(io.Discard, ArchiveOptions{ServerVersion: serverVersion})
		if err != nil {
			t.Fatal(err)
		}
		var want [][]byte
		write := func(doc []byte) {
			if bson.Raw(doc).Validate() == nil {
				if werr := a.WriteEntry(doc); werr != nil {
					t.Fatalf("WriteEntry refused a valid document: %v", werr)
				}
				want = append(want, append([]byte(nil), doc...))
				return
			}
			if werr := side.WriteEntry(doc); werr != nil && !errors.Is(werr, ErrMalformed) {
				t.Fatalf("WriteEntry: %v, want nil or ErrMalformed", werr)
			}
		}
		rest := data
		r := NewReader(bytes.NewReader(data))
		for {
			doc, rerr := r.Next()
			if rerr != nil {
				break
			}
			write(doc)
			rest = rest[len(doc):]
		}
		if len(rest) > 0 {
			write(rest)
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		if a.Entries() != int64(len(want)) {
			t.Fatalf("Entries() = %d, want %d", a.Entries(), len(want))
		}

		out := bytes.NewReader(buf.Bytes())
		prelude, err := mongotools.ReadArchivePrelude(out, 0)
		if err != nil {
			t.Fatalf("the archive's prelude does not read back: %v", err)
		}
		wantTool := toolVersion
		if wantTool == "" {
			wantTool = DefaultToolVersion
		}
		if prelude.ServerVersion != serverVersion || prelude.ToolVersion != wantTool {
			t.Fatalf("prelude versions %q, %q; want %q, %q", prelude.ServerVersion, prelude.ToolVersion, serverVersion, wantTool)
		}
		body := readArchiveBody(t, io.Reader(out))
		if body.segments != 1 || len(body.entries) != len(want) {
			t.Fatalf("%d segments, %d entries; want 1 segment, %d entries", body.segments, len(body.entries), len(want))
		}
		for i := range want {
			if !bytes.Equal(body.entries[i], want[i]) {
				t.Fatalf("entry %d changed", i)
			}
		}
	})
}
