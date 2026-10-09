package mongoconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// silentMember is a fake replica set member on loopback that answers the
// handshake of every connection and then never answers again, like a member that
// crashed or was cut off without closing its connections (no RST): reads block
// until a deadline ends them.
type silentMember struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newSilentMember(t *testing.T) *silentMember {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &silentMember{ln: ln}
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
	return "mongodb://" + m.ln.Addr().String() + "/?directConnection=true"
}

// serve answers the first message of c (the handshake) as a primary of rs0 and
// reads everything else without answering.
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
		if _, err := io.CopyN(io.Discard, c, int64(length-16)); err != nil {
			return
		}
		if !first {
			continue // the member stopped answering
		}
		doc, err := bson.Marshal(bson.D{
			{Key: "ok", Value: 1}, {Key: "isWritablePrimary", Value: true}, {Key: "ismaster", Value: true},
			{Key: "helloOk", Value: true}, {Key: "setName", Value: "rs0"}, {Key: "minWireVersion", Value: 0},
			{Key: "maxWireVersion", Value: 21}, {Key: "maxBsonObjectSize", Value: 16 << 20},
			{Key: "maxMessageSizeBytes", Value: 48000000}, {Key: "maxWriteBatchSize", Value: 100000},
			{Key: "localTime", Value: bson.DateTime(time.Now().UnixMilli())},
		})
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
		body.Write(doc)
		out := make([]byte, 16, 16+body.Len())
		binary.LittleEndian.PutUint32(out[0:4], uint32(16+body.Len())) //nolint:gosec // a small reply
		binary.LittleEndian.PutUint32(out[4:8], requestID+1)
		binary.LittleEndian.PutUint32(out[8:12], requestID)
		binary.LittleEndian.PutUint32(out[12:16], replyOp)
		if _, err = c.Write(append(out, body.Bytes()...)); err != nil {
			return
		}
	}
}

// TestOplogSessionGivesUpOnASilentMember is the regression test of a collector
// that hung for minutes when the member it read from was killed without closing
// its connections (found by the replica set divergence scenario): every round
// trip of an oplog session to a member now has a deadline, so the collector's
// tick fails, logs and tries again instead of waiting forever.
func TestOplogSessionGivesUpOnASilentMember(t *testing.T) {
	old := memberOpTimeout
	memberOpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { memberOpTimeout = old })
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
