package oplog

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// writeSplit writes b to w in random pieces (empty pieces included).
func writeSplit(t testing.TB, w io.Writer, b []byte, rng *rand.Rand) error {
	t.Helper()
	for len(b) > 0 {
		n := rng.IntN(min(len(b), 64) + 1)
		if rng.IntN(10) == 0 {
			n = min(len(b), rng.IntN(4096))
		}
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func TestScannerRandomSplits(t *testing.T) {
	for _, v := range fixtureVersions {
		for scenario := range fixtureScenarios {
			data := readFixture(t, v, scenario)
			entries := splitEntries(t, data)
			wantFirst, err := entryOpTime(entries[0])
			if err != nil {
				t.Fatal(err)
			}
			wantLast, err := entryOpTime(entries[len(entries)-1])
			if err != nil {
				t.Fatal(err)
			}
			for seed := uint64(0); seed < 20; seed++ {
				var out bytes.Buffer
				s := NewScanner(&out)
				if err := writeSplit(t, s, data, rand.New(rand.NewPCG(seed, 7))); err != nil { //nolint:gosec // G404: reproducible write splits, not secrets.
					t.Fatalf("%s/%s seed %d: write: %v", v, scenario, seed, err)
				}
				if err := s.Finish(); err != nil {
					t.Fatalf("%s/%s seed %d: finish: %v", v, scenario, seed, err)
				}
				if !bytes.Equal(out.Bytes(), data) {
					t.Fatalf("%s/%s seed %d: bytes changed on the way through", v, scenario, seed)
				}
				if s.Count() != int64(len(entries)) || s.First() != wantFirst || s.Last() != wantLast {
					t.Fatalf("%s/%s seed %d: count %d first %+v last %+v, want %d %+v %+v",
						v, scenario, seed, s.Count(), s.First(), s.Last(), len(entries), wantFirst, wantLast)
				}
			}
		}
	}
}

func TestScannerRecordsTerm(t *testing.T) {
	a := entryAt(t, ts(10, 1), bson.D{{Key: "op", Value: "n"}, {Key: "ns", Value: ""}, {Key: "o", Value: bson.D{}}})
	noTerm := mustMarshal(t, bson.D{{Key: "op", Value: "n"}, {Key: "ts", Value: ts(10, 2)}})
	s := NewScanner(nil)
	if _, err := s.Write(append(append([]byte{}, a...), noTerm...)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := s.First(); got != (OpTime{TS: ts(10, 1), T: 1}) {
		t.Errorf("first = %+v", got)
	}
	if got := s.Last(); got != (OpTime{TS: ts(10, 2), T: -1}) {
		t.Errorf("last = %+v, want term -1 for an entry without t", got)
	}
}

func TestScannerEmpty(t *testing.T) {
	s := NewScanner(nil)
	if err := s.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if s.Count() != 0 || s.First() != (OpTime{}) || s.Last() != (OpTime{}) {
		t.Fatalf("empty stream: count %d first %+v last %+v", s.Count(), s.First(), s.Last())
	}
}

func TestScannerTruncated(t *testing.T) {
	data := readFixture(t, "8.0", "crud")
	for _, cut := range []int{1, 3, 4, 5, len(data) / 2, len(data) - 1} {
		s := NewScanner(nil)
		if _, err := s.Write(data[:cut]); err != nil {
			t.Fatalf("cut %d: write: %v", cut, err)
		}
		if err := s.Finish(); !errors.Is(err, ErrTruncated) {
			t.Fatalf("cut %d: finish = %v, want ErrTruncated", cut, err)
		}
	}
}

func TestScannerMalformed(t *testing.T) {
	valid := entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "n"}})
	noTS := mustMarshal(t, bson.D{{Key: "op", Value: "n"}})
	corrupt := append([]byte{}, valid...)
	corrupt[4] = 0x7e // an unknown element type
	cases := map[string][]byte{
		"length below 5":     {4, 0, 0, 0},
		"length above limit": {0xff, 0xff, 0xff, 0x7f},
		"no ts":              noTS,
		"invalid element":    corrupt,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			s := NewScanner(&out)
			_, err := s.Write(append(append([]byte{}, valid...), data...))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("write = %v, want ErrMalformed", err)
			}
			if out.Len() != 0 {
				t.Errorf("%d bytes written through despite the error", out.Len())
			}
			if _, err := s.Write(valid); !errors.Is(err, ErrMalformed) {
				t.Errorf("later write = %v, want the same error", err)
			}
			if err := s.Finish(); !errors.Is(err, ErrMalformed) {
				t.Errorf("finish = %v, want ErrMalformed", err)
			}
		})
	}
}

// failWriter fails every write.
type failWriter struct{}

var errWrite = errors.New("write failed")

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestScannerWriterError(t *testing.T) {
	s := NewScanner(failWriter{})
	if _, err := s.Write(entryAt(t, ts(1, 1), bson.D{{Key: "op", Value: "n"}})); !errors.Is(err, errWrite) {
		t.Fatalf("write = %v, want the writer's error", err)
	}
	if err := s.Finish(); !errors.Is(err, errWrite) {
		t.Fatalf("finish = %v, want the writer's error", err)
	}
}

func TestReaderNext(t *testing.T) {
	data := readFixture(t, "5.0", "txn")
	r := NewReader(bytes.NewReader(data[:len(data)-2]))
	var n int
	var err error
	for {
		if _, err = r.Next(); err != nil {
			break
		}
		n++
	}
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated stream: err = %v after %d entries, want ErrTruncated", err, n)
	}
	if _, err := NewReader(bytes.NewReader([]byte{1, 0, 0, 0})).Next(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("length 1: err = %v, want ErrMalformed", err)
	}
	if _, err := NewReader(bytes.NewReader([]byte{9, 0})).Next(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("partial prefix: err = %v, want ErrTruncated", err)
	}
	if _, err := NewReader(failReader{}).Next(); !errors.Is(err, errRead) {
		t.Fatalf("read error: err = %v, want it wrapped", err)
	}
}

// failReader fails every read.
type failReader struct{}

var errRead = errors.New("read failed")

func (failReader) Read([]byte) (int, error) { return 0, errRead }
