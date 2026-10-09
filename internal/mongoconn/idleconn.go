package mongoconn

import (
	"context"
	"net"
	"sync"
	"time"
)

// memberIdleTimeout is how long a read on an oplog session's connection may wait
// without receiving a single byte. It bounds the oplog range reads by progress,
// not by duration: a batch of 16 MiB over a slow link takes as long as it takes
// while bytes arrive, but a member that stops sending (it died or was cut off
// without closing the connection) fails the read after this long. A variable for
// tests.
var memberIdleTimeout = time.Minute

// idleDialer dials the connections of an oplog session as the driver's default
// dialer does, wrapped in idleConn.
type idleDialer struct{ net.Dialer }

// newIdleDialer returns an idleDialer with the driver's default keep-alive.
func newIdleDialer() *idleDialer {
	return &idleDialer{Dialer: net.Dialer{KeepAlive: 300 * time.Second}}
}

// DialContext dials address and wraps the connection in idleConn.
func (d *idleDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.Dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &idleConn{Conn: c, idle: memberIdleTimeout}, nil
}

// idleConn is a net.Conn whose reads time out after idle without any byte, on
// top of the read deadline the driver sets (from the operation's context): every
// Read moves the effective deadline to the earlier of the driver's deadline and
// now plus idle. TLS runs on top of it, so encrypted connections are covered.
type idleConn struct {
	net.Conn
	idle time.Duration

	mu       sync.Mutex
	deadline time.Time // the read deadline the driver asked for; zero for none
}

// effective returns the read deadline to apply now for the driver's deadline d.
func (c *idleConn) effective(d time.Time) time.Time {
	next := time.Now().Add(c.idle)
	if !d.IsZero() && d.Before(next) {
		return d
	}
	return next
}

// SetReadDeadline records the driver's deadline and applies it at once (capped
// by the idle timeout), so a deadline in the past still interrupts a read in
// flight, as the driver does on cancellation.
func (c *idleConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return c.Conn.SetReadDeadline(c.effective(t))
}

// SetDeadline sets the write deadline and the read deadline (see
// SetReadDeadline).
func (c *idleConn) SetDeadline(t time.Time) error {
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return c.SetReadDeadline(t)
}

// Read reads with a deadline of idle from now (or the driver's, if earlier).
// The deadline is applied under the lock, so a deadline set concurrently (a
// cancellation) is never overwritten by a stale one.
func (c *idleConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	err := c.Conn.SetReadDeadline(c.effective(c.deadline))
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}
