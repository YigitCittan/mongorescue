package mongoconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// silentMember is a fake replica set member on loopback. Silent (trickle zero),
// it answers the handshake of every connection and then never answers again,
// like a member that crashed or was cut off without closing its connections (no
// RST): reads block until a deadline ends them. With trickle, it answers every
// hello at once and a find on the oplog with two large entries sent a few bytes
// every trickle, like a slow link that keeps delivering.
type silentMember struct {
	ln      net.Listener
	trickle time.Duration
	mu      sync.Mutex
	conns   []net.Conn
}

func newSilentMember(t *testing.T) *silentMember {
	return newFakeMember(t, 0)
}

func newFakeMember(t *testing.T, trickle time.Duration) *silentMember {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &silentMember{ln: ln, trickle: trickle}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			m.mu.Lock()
			m.conns = append(m.conns, c)
			m.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		m.mu.Lock()
		for _, c := range m.conns {
			_ = c.Close()
		}
		m.mu.Unlock()
		wg.Wait()
	})
	return m
}

// uri returns a direct connection string of the member.
func (m *silentMember) uri() string {
	return "mongodb://" + m.ln.Addr().String() + "/?directConnection=true&serverSelectionTimeoutMS=5000"
}

// serve answers the messages of c: the first (the handshake) always, the others
// only with trickle (see silentMember).
func (m *silentMember) serve(c net.Conn) {
	hdr := make([]byte, 16)
	for first := true; ; first = false {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		length := binary.LittleEndian.Uint32(hdr[0:4])
		requestID := binary.LittleEndian.Uint32(hdr[4:8])
		opCode := binary.LittleEndian.Uint32(hdr[12:16])
		if length < 16 || length > 48<<20 {
			return
		}
		body := make([]byte, length-16)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		switch {
		case first:
			m.reply(c, requestID, opCode, fakeHello(), 0)
		case m.trickle == 0:
			// The member stopped answering.
		case opCode == 2013 && commandOf(body) == "find":
			m.reply(c, requestID, opCode, findReply(), m.trickle)
		case opCode == 2013 && (commandOf(body) == "hello" || commandOf(body) == "isMaster" || commandOf(body) == "ismaster"):
			m.reply(c, requestID, opCode, fakeHello(), 0)
		default:
			m.reply(c, requestID, opCode, bson.D{{Key: "ok", Value: 1}}, 0)
		}
	}
}

// commandOf returns the command name of an OP_MSG body: the first key of its
// body section.
func commandOf(body []byte) string {
	if len(body) < 5 || body[4] != 0 {
		return ""
	}
	elems, err := bson.Raw(body[5:]).Elements()
	if err != nil || len(elems) == 0 {
		return ""
	}
	return elems[0].Key()
}

// fakeHello is the answer of a primary of rs0.
func fakeHello() bson.D {
	return bson.D{
		{Key: "ok", Value: 1}, {Key: "isWritablePrimary", Value: true}, {Key: "ismaster", Value: true},
		{Key: "helloOk", Value: true}, {Key: "setName", Value: "rs0"}, {Key: "minWireVersion", Value: 0},
		{Key: "maxWireVersion", Value: 21}, {Key: "maxBsonObjectSize", Value: 16 << 20},
		{Key: "maxMessageSizeBytes", Value: 48000000}, {Key: "maxWriteBatchSize", Value: 100000},
		{Key: "localTime", Value: bson.DateTime(time.Now().UnixMilli())},
	}
}

// findReply is a cursor over the oplog entries at 10:1 and 11:1 (term 1), each
// with 2 KiB of padding, in one batch.
func findReply() bson.D {
	pad := strings.Repeat("x", 2048)
	entry := func(sec uint32) bson.D {
		return bson.D{{Key: "op", Value: "n"}, {Key: "ns", Value: ""}, {Key: "o", Value: bson.D{{Key: "pad", Value: pad}}},
			{Key: "ts", Value: bson.Timestamp{T: sec, I: 1}}, {Key: "t", Value: int64(1)}}
	}
	return bson.D{{Key: "cursor", Value: bson.D{{Key: "firstBatch", Value: bson.A{entry(10), entry(11)}},
		{Key: "id", Value: int64(0)}, {Key: "ns", Value: "local.oplog.rs"}}}, {Key: "ok", Value: 1}}
}

// reply sends doc in reply to requestID (OP_REPLY to an OP_QUERY handshake,
// OP_MSG otherwise), 64 bytes every trickle when trickle is set.
func (m *silentMember) reply(c net.Conn, requestID, opCode uint32, doc bson.D, trickle time.Duration) {
	raw, err := bson.Marshal(doc)
	if err != nil {
		return
	}
	var body bytes.Buffer
	replyOp := uint32(2013) // OP_MSG
	switch opCode {
	case 2004: // OP_QUERY handshake: OP_REPLY
		replyOp = 1
		_ = binary.Write(&body, binary.LittleEndian, struct {
			Flags    int32
			CursorID int64
			From     int32
			Returned int32
		}{0, 0, 0, 1})
	case 2013:
		_ = binary.Write(&body, binary.LittleEndian, uint32(0))
		body.WriteByte(0) // section kind 0: the body document
	default:
		return
	}
	body.Write(raw)
	out := make([]byte, 16, 16+body.Len())
	binary.LittleEndian.PutUint32(out[0:4], uint32(16+body.Len())) //nolint:gosec // a small reply
	binary.LittleEndian.PutUint32(out[4:8], requestID+1)
	binary.LittleEndian.PutUint32(out[8:12], requestID)
	binary.LittleEndian.PutUint32(out[12:16], replyOp)
	out = append(out, body.Bytes()...)
	if trickle == 0 {
		_, _ = c.Write(out)
		return
	}
	for len(out) > 0 {
		n := min(64, len(out))
		if _, err = c.Write(out[:n]); err != nil {
			return
		}
		out = out[n:]
		time.Sleep(trickle)
	}
}

// TestOplogSessionGivesUpOnASilentMember is the regression test of a collector
// that hung for minutes when the member it read from was killed without closing
// its connections (found by the replica set divergence scenario): every round
// trip of an oplog session to a member now has a deadline, so the collector's
// tick fails, logs and tries again instead of waiting forever.
func TestOplogSessionGivesUpOnASilentMember(t *testing.T) {
	oldOp, oldIdle := memberOpTimeout, memberIdleTimeout
	memberOpTimeout, memberIdleTimeout = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { memberOpTimeout, memberIdleTimeout = oldOp, oldIdle })
	m := newSilentMember(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := New().OpenOplogSession(ctx, m.uri(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	within := func(what string, fn func() error) {
		t.Helper()
		start := time.Now()
		err := fn()
		if err == nil {
			t.Fatalf("%s: no error from a silent member", what)
		}
		if took := time.Since(start); took > 20*time.Second {
			t.Fatalf("%s took %s", what, took)
		}
	}
	within("Pin", func() error { _, pinErr := s.Pin(ctx, pitr.Timestamp{}); return pinErr })
	p := s.Primary()
	within("OplogWindow", func() error { _, winErr := p.OplogWindow(ctx); return winErr })
	within("EntryAt", func() error { _, _, entryErr := p.EntryAt(ctx, pitr.Timestamp{T: 1}); return entryErr })
	within("EntryAtOrAfter", func() error { _, _, entryErr := p.EntryAtOrAfter(ctx, pitr.Timestamp{T: 1}); return entryErr })
	within("ReadOplog", func() error {
		_, readErr := p.ReadOplog(ctx, pitr.OplogRange{From: pitr.Timestamp{T: 1}, To: pitr.Timestamp{T: 2}}, io.Discard)
		return readErr
	})
	if ctx.Err() != nil {
		t.Fatal("the calls ran into the test's own deadline")
	}
}

// TestOplogReadOutlastsTheTimeoutsWhileBytesArrive checks that range reads are
// bounded by progress, not by duration: a member that sends a 4 KiB batch 64
// bytes every 20 ms (over a second in all, like a 16 MiB batch on a slow
// cross-region link) is read to the end although the round-trip and idle
// timeouts are 300 ms, while a silent member fails (see above).
func TestOplogReadOutlastsTheTimeoutsWhileBytesArrive(t *testing.T) {
	oldOp, oldIdle := memberOpTimeout, memberIdleTimeout
	memberOpTimeout, memberIdleTimeout = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { memberOpTimeout, memberIdleTimeout = oldOp, oldIdle })
	m := newFakeMember(t, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := New().OpenOplogSession(ctx, m.uri(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	var buf bytes.Buffer
	stats, err := s.Primary().ReadOplog(ctx, pitr.OplogRange{From: pitr.Timestamp{T: 10, I: 1}, To: pitr.Timestamp{T: 11, I: 1},
		CheckTerm: true, FromTerm: 1}, &buf)
	if err != nil {
		t.Fatalf("a slow but steady read failed after %s: %v", time.Since(start), err)
	}
	if took := time.Since(start); took < 3*memberIdleTimeout || stats.Entries != 1 || stats.Last.TS != (pitr.Timestamp{T: 11, I: 1}) {
		t.Fatalf("read %+v in %s; want one entry, slower than the timeouts", stats, took)
	}
}
