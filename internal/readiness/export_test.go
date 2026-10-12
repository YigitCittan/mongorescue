package readiness

import "time"

// FakeLoopTimer is a checker loop timer that fires only when the test calls Fire
// and reports every Reset on Resets.
type FakeLoopTimer struct {
	c      chan time.Time
	Resets chan time.Duration
}

// C implements loopTimer.
func (f *FakeLoopTimer) C() <-chan time.Time { return f.c }

// Stop implements loopTimer: the timer has not fired unless the test fired it.
func (f *FakeLoopTimer) Stop() bool { return true }

// Reset implements loopTimer.
func (f *FakeLoopTimer) Reset(d time.Duration) { f.Resets <- d }

// Fire makes the timer fire.
func (f *FakeLoopTimer) Fire() { f.c <- time.Time{} }

// UseFakeLoop gives the checker loop of s a fake timer and the monotonic clock
// mono; call it before Start.
func UseFakeLoop(s *Service, mono func() time.Time) *FakeLoopTimer {
	f := &FakeLoopTimer{c: make(chan time.Time, 1), Resets: make(chan time.Duration, 1024)}
	s.newTimer = func(time.Duration) loopTimer { return f }
	s.mono = mono
	return f
}
