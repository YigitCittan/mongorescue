package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultStallTimeout is the upload stall timeout (see StallWatch) when none is
// configured.
const DefaultStallTimeout = 5 * time.Minute

// ErrStorageStalled indicates that an upload made no progress on the storage side
// for the stall timeout (a network partition or a hung disk) and was cancelled.
var ErrStorageStalled = errors.New("storage: upload stalled")

// StallWatch configures the upload watchdog of a driver: an upload whose storage side
// accepts no bytes for the stall timeout is cancelled and fails with an error
// wrapping ErrStorageStalled. Every driver's Save starts it (see Start), so no upload
// path can go without it.
type StallWatch struct {
	// Target names the storage target in the error.
	Target string
	// Timeout returns the stall timeout, read at the start of each upload so a
	// changed setting applies to the next upload; nil or a non-positive value means
	// DefaultStallTimeout.
	Timeout func() time.Duration
}

// timeout returns the stall timeout of the next upload.
func (w StallWatch) timeout() time.Duration {
	if w.Timeout != nil {
		if d := w.Timeout(); d > 0 {
			return d
		}
	}
	return DefaultStallTimeout
}

// Start starts the progress watchdog of one upload of r. The driver must run the
// upload with the returned context and reader and pass its result through finish,
// which stops the watchdog (waiting for its goroutine) and, when the watchdog
// cancelled a failed upload, returns an error wrapping ErrStorageStalled in place of
// the cancellation it caused. An upload that succeeds anyway keeps its success.
//
// Progress is any byte the uploader reads from r and, for HTTP drivers (see
// progressClient), any request body byte sent or response byte received. Time the
// uploader spends inside a Read of r, waiting for a slow or throttled source, never
// counts as a stall: the source has its own stall check.
func (w StallWatch) Start(ctx context.Context, r io.Reader) (context.Context, io.Reader, func(error) error) {
	timeout := w.timeout()
	u := &uploadWatch{start: time.Now()}
	wctx, cancel := context.WithCancelCause(ctx)
	wctx = context.WithValue(wctx, uploadWatchKey{}, u)
	stalled := fmt.Errorf("%w: no upload progress for %s to target %q", ErrStorageStalled, timeout, w.Target)

	var fired atomic.Bool
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(min(max(timeout/10, 10*time.Millisecond), 5*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-wctx.Done():
				return
			case <-ticker.C:
				if u.idle() > timeout {
					fired.Store(true)
					cancel(stalled)
					return
				}
			}
		}
	}()

	var once sync.Once
	finish := func(err error) error {
		once.Do(func() {
			close(done)
			wg.Wait()
			cancel(nil)
		})
		if err != nil && fired.Load() {
			return stalled
		}
		return err
	}
	return wctx, &watchedReader{r: r, u: u}, finish
}

// uploadWatchKey is the context key of the uploadWatch of an upload.
type uploadWatchKey struct{}

// uploadWatch tracks the progress of one upload on the monotonic clock.
type uploadWatch struct {
	start time.Time
	// last is the time of the last progress, as a monotonic offset from start.
	last atomic.Int64
	// reading counts Reads of the source in progress.
	reading atomic.Int32
}

// touch records progress.
func (u *uploadWatch) touch() { u.last.Store(int64(time.Since(u.start))) }

// idle returns how long the upload has made no progress, or 0 while the uploader
// waits for the source.
func (u *uploadWatch) idle() time.Duration {
	if u.reading.Load() > 0 {
		return 0
	}
	return time.Since(u.start) - time.Duration(u.last.Load())
}

// watchedReader reports the reads of an upload's source to its uploadWatch.
type watchedReader struct {
	r io.Reader
	u *uploadWatch
}

// Read reads from the source; the time spent in it is not a stall.
func (w *watchedReader) Read(p []byte) (int, error) {
	w.u.touch()
	w.u.reading.Add(1)
	n, err := w.r.Read(p)
	w.u.touch()
	w.u.reading.Add(-1)
	return n, err
}

// httpDoer is the HTTP client interface of the AWS SDK.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// progressClient reports the bytes an HTTP driver sends and receives to the
// uploadWatch in the request's context, so a part upload that is slow but moving is
// not a stall and one stuck on the network is.
type progressClient struct {
	next httpDoer
}

// Do sends req through the wrapped client, counting body bytes as progress.
func (c progressClient) Do(req *http.Request) (*http.Response, error) {
	u, _ := req.Context().Value(uploadWatchKey{}).(*uploadWatch)
	if u == nil {
		return c.next.Do(req)
	}
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = &progressBody{ReadCloser: req.Body, u: u}
	}
	resp, err := c.next.Do(req)
	u.touch()
	if resp != nil && resp.Body != nil {
		resp.Body = &progressBody{ReadCloser: resp.Body, u: u}
	}
	return resp, err
}

// progressBody records progress for every byte read through it.
type progressBody struct {
	io.ReadCloser
	u *uploadWatch
}

// Read reads from the body and records progress.
func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.u.touch()
	}
	return n, err
}
