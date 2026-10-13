package keywrap

import (
	"context"
	"sync"
	"time"
)

// Operations reported to an Observer.
const (
	// OpWrap is a Wrapper.Wrap call.
	OpWrap = "wrap"
	// OpUnwrap is a Wrapper.Unwrap call.
	OpUnwrap = "unwrap"
)

// Observer receives the outcome of every KMS call: the operation, the provider,
// how long it took and its error (nil on success). It must not block.
type Observer func(op, provider string, d time.Duration, err error)

// maxPending bounds the calls an Instrumented keeps until an Observer is set.
const maxPending = 64

// observation is a call made before the Observer was set.
type observation struct {
	op  string
	d   time.Duration
	err error
}

// Instrumented is a Wrapper that reports every call to an Observer. Calls made
// before SetObserver (the unwrap at startup, which precedes the metrics) are kept
// and replayed. It is safe for concurrent use.
type Instrumented struct {
	w       Wrapper
	mu      sync.Mutex
	obs     Observer
	pending []observation
}

// Instrument returns w reporting to an Observer set later.
func Instrument(w Wrapper) *Instrumented { return &Instrumented{w: w} }

// SetObserver sets the observer and replays the calls made before.
func (i *Instrumented) SetObserver(o Observer) {
	i.mu.Lock()
	pending := i.pending
	i.pending, i.obs = nil, o
	i.mu.Unlock()
	if o == nil {
		return
	}
	for _, p := range pending {
		o(p.op, i.w.Provider(), p.d, p.err)
	}
}

// observe reports one call.
func (i *Instrumented) observe(op string, d time.Duration, err error) {
	i.mu.Lock()
	o := i.obs
	if o == nil && len(i.pending) < maxPending {
		i.pending = append(i.pending, observation{op: op, d: d, err: err})
	}
	i.mu.Unlock()
	if o != nil {
		o(op, i.w.Provider(), d, err)
	}
}

// Provider returns the provider of the wrapped Wrapper.
func (i *Instrumented) Provider() string { return i.w.Provider() }

// KeyID returns the key ID of the wrapped Wrapper.
func (i *Instrumented) KeyID() string { return i.w.KeyID() }

// Wrap calls the wrapped Wrapper and reports the call.
func (i *Instrumented) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	start := time.Now()
	ct, err := i.w.Wrap(ctx, key)
	i.observe(OpWrap, time.Since(start), err)
	return ct, err
}

// Unwrap calls the wrapped Wrapper and reports the call.
func (i *Instrumented) Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error) {
	start := time.Now()
	key, err := i.w.Unwrap(ctx, ciphertext)
	i.observe(OpUnwrap, time.Since(start), err)
	return key, err
}
