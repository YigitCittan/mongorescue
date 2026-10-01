package runlog

import (
	"errors"
	"io"
)

// part is one piece of a log: a file (or the marker) read up to size.
type part struct {
	r    io.ReaderAt
	size int64
}

// Reader reads a log, possibly assembled from the parts of a running log. It is not
// safe for concurrent use.
type Reader struct {
	parts   []part
	closers []io.Closer
}

// Size returns the length of the log in bytes.
func (r *Reader) Size() int64 {
	var n int64
	for _, p := range r.parts {
		n += p.size
	}
	return n
}

// ReadAt implements io.ReaderAt over the concatenated parts.
func (r *Reader) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("runlog: negative offset")
	}
	n := 0
	for _, p := range r.parts {
		if len(b) == 0 {
			break
		}
		if off >= p.size {
			off -= p.size
			continue
		}
		want := min(int64(len(b)), p.size-off)
		got, err := p.r.ReadAt(b[:want], off)
		n += got
		if err != nil && !(errors.Is(err, io.EOF) && int64(got) == want) {
			if errors.Is(err, io.EOF) {
				// The part is shorter than recorded (a file rotated meanwhile).
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
		b = b[got:]
		off = 0
	}
	if len(b) > 0 {
		return n, io.EOF
	}
	return n, nil
}

// WriteTo streams the whole log to w.
func (r *Reader) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, io.NewSectionReader(r, 0, r.Size()))
}

// Close releases the files of the log.
func (r *Reader) Close() error {
	var errs []error
	for _, c := range r.closers {
		errs = append(errs, c.Close())
	}
	r.closers = nil
	return errors.Join(errs...)
}

// tailChunk is how much Tail reads per step, backwards from the end.
const tailChunk = 64 << 10

// Tail returns the last n lines of the log (at most MaxTailLines). Memory is bounded
// by the size of the returned lines.
func (r *Reader) Tail(n int) ([]byte, error) {
	if n <= 0 {
		return []byte{}, nil
	}
	n = min(n, MaxTailLines)
	size := r.Size()
	end := size
	// A final newline ends the last line; it does not start an empty one.
	var last [1]byte
	if size > 0 {
		if _, err := r.ReadAt(last[:], size-1); err != nil {
			return nil, err
		}
	}
	newlines := 0
	if size > 0 && last[0] == '\n' {
		newlines = -1
	}
	start := end
	buf := make([]byte, tailChunk)
	for start > 0 {
		step := min(int64(tailChunk), start)
		chunk := buf[:step]
		if _, err := r.ReadAt(chunk, start-step); err != nil {
			return nil, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				continue
			}
			newlines++
			if newlines == n {
				start = start - step + int64(i) + 1
				return r.read(start, end)
			}
		}
		start -= step
	}
	return r.read(0, end)
}

// read returns bytes [from, to) of the log.
func (r *Reader) read(from, to int64) ([]byte, error) {
	out := make([]byte, to-from)
	if _, err := r.ReadAt(out, from); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}
