package throttle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// fakeClock advances only when the reader sleeps, plus step per Now call (the time
// the reads themselves take).
type fakeClock struct {
	now   time.Time
	step  time.Duration
	slept time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	c.now = c.now.Add(d)
	c.slept += d
	return nil
}

// chunkReader returns at most size bytes per Read, like a pipe.
type chunkReader struct {
	r    io.Reader
	size int
}

func (c chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.size {
		p = p[:c.size]
	}
	return c.r.Read(p)
}

func TestReaderStaysWithinTheRate(t *testing.T) {
	cases := []struct {
		name  string
		rate  float64 // bytes per second
		total int
		chunk int
		step  time.Duration
	}{
		{"1 MB/s in 32 KiB reads", 1e6, 20e6, 32 << 10, 0},
		{"12.5 MB/s (100 Mbit/s) in 1 MiB reads", 12.5e6, 200e6, 1 << 20, 0},
		{"125 kB/s (1 Mbit/s) in 4 KiB reads", 125e3, 2e6, 4 << 10, 0},
		{"slow source below the rate", 1e6, 5e6, 64 << 10, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1_700_000_000, 0), step: tc.step}
			start := clock.now
			src := chunkReader{r: bytes.NewReader(make([]byte, tc.total)), size: tc.chunk}
			r := NewReader(context.Background(), src, tc.rate, WithClock(clock))
			n, err := io.Copy(io.Discard, r)
			if err != nil || n != int64(tc.total) {
				t.Fatalf("copied %d, %v", n, err)
			}
			elapsed := clock.now.Sub(start).Seconds()
			got := float64(n) / elapsed
			if tc.step == 0 {
				if got < tc.rate*0.9 || got > tc.rate*1.1 {
					t.Fatalf("rate %.0f B/s, want %.0f ±10%%", got, tc.rate)
				}
				return
			}
			// A source slower than the rate is never slowed down further.
			if got > tc.rate*1.1 {
				t.Fatalf("rate %.0f B/s above %.0f", got, tc.rate)
			}
		})
	}
}

// Over every window the bytes passed stay within rate*window plus one bucket.
func TestReaderNeverBurstsBeyondTheBucket(t *testing.T) {
	const rate, burst = 1e6, 100_000
	clock := &fakeClock{now: time.Unix(0, 0)}
	r := NewReader(context.Background(), bytes.NewReader(make([]byte, 5e6)), rate, WithClock(clock), WithBurst(burst))
	buf := make([]byte, 1<<20)
	start := clock.now
	var total int
	for {
		n, err := r.Read(buf)
		if n > burst {
			t.Fatalf("one read returned %d bytes, more than the bucket", n)
		}
		total += n
		if limit := rate*clock.now.Sub(start).Seconds() + burst; float64(total) > limit+1 {
			t.Fatalf("%d bytes after %v, limit %.0f", total, clock.now.Sub(start), limit)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestReaderUnlimited(t *testing.T) {
	src := bytes.NewReader(nil)
	if r := NewReader(context.Background(), src, 0); r != io.Reader(src) {
		t.Fatal("a zero rate must return the source itself")
	}
}

func TestReaderStopsWhenTheContextEnds(t *testing.T) {
	cause := errors.New("run cancelled")
	ctx, cancel := context.WithCancelCause(context.Background())
	// 1 byte per second with a 1-byte bucket: the second byte waits a second.
	r := NewReader(ctx, bytes.NewReader([]byte("abc")), 1, WithBurst(1))
	buf := make([]byte, 8)
	if n, err := r.Read(buf); n != 1 || err != nil {
		t.Fatalf("first read: %d, %v", n, err)
	}
	time.AfterFunc(20*time.Millisecond, func() { cancel(cause) })
	begin := time.Now()
	_, err := r.Read(buf)
	if !errors.Is(err, cause) {
		t.Fatalf("read during the wait: %v, want the context's cause", err)
	}
	if waited := time.Since(begin); waited > 900*time.Millisecond {
		t.Fatalf("waited %v after the cancellation", waited)
	}
	if _, err = r.Read(buf); !errors.Is(err, cause) {
		t.Fatalf("read after the cancellation: %v", err)
	}
}
