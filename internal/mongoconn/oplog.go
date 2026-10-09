package mongoconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// DefaultOplogReadPreference is the read preference of oplog reads unless a stream
// sets another: the collector reads from a secondary when one is available.
const DefaultOplogReadPreference = "secondaryPreferred"

// oplogDB and oplogColl name the replica set oplog.
const (
	oplogDB   = "local"
	oplogColl = "oplog.rs"
)

// ErrInvalidReadPreference is returned by OpenOplogSession for an unknown read
// preference mode.
var ErrInvalidReadPreference = errors.New("mongoconn: invalid read preference")

// memberOpTimeout bounds one round trip of an oplog session to a member (and one
// call of OplogWindow, a few round trips). The driver has no socket timeout of
// its own, so a member that stops answering without closing its connections (a
// crash, a power loss, a network partition) would otherwise hold the collector's
// tick until the operating system gives up on the connection, minutes later.
// A variable for tests.
var memberOpTimeout = time.Minute

// memberCtx returns ctx bounded by memberOpTimeout.
func memberCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, memberOpTimeout)
}

// OplogSession is one long-lived client to a replica set, opened once per PITR
// stream so the collector does not reconnect at every tick. Its own reads (the
// promoted OplogMember methods) use the session's read preference, so each one
// may be served by another member; Pin returns a reader bound to one member, and
// Primary one bound to the primary. It is safe for concurrent use; Close
// disconnects it and the clients of the pinned members.
type OplogSession struct {
	OplogMember
	uri string
	// tls is the TLS material of the connection, reused for member clients.
	tls *models.ConnectionTLS

	mu      sync.Mutex
	members map[string]*mongo.Client
}

// OplogMember reads the oplog of one replica set member, or of the members its
// read preference selects (an OplogSession's own reads). Errors are redacted.
type OplogMember struct {
	client *mongo.Client
	rp     *readpref.ReadPref
	// majority reads the oplog with read concern majority (the primary's reader).
	majority bool
	// host is the member's address; empty when the read preference selects it.
	host string
}

// Host returns the address of the member the reader is bound to, or "" when its
// read preference selects a member for each read.
func (s *OplogMember) Host() string { return s.host }

// oplog returns the oplog collection with the reader's read concern.
func (s *OplogMember) oplog() *mongo.Collection {
	opts := options.Database()
	if s.majority {
		opts.SetReadConcern(readconcern.Majority())
	}
	return s.client.Database(oplogDB, opts).Collection(oplogColl)
}

// OpenOplogSession returns a session for uri reading with readPreference (empty
// means DefaultOplogReadPreference). The connection string's own timeouts apply,
// capped by ctx's deadline at connect time. Errors never quote the URI.
func (p *Prober) OpenOplogSession(ctx context.Context, uri, readPreference string) (*OplogSession, error) {
	if readPreference == "" {
		readPreference = DefaultOplogReadPreference
	}
	mode, err := readpref.ModeFromString(readPreference)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidReadPreference, readPreference)
	}
	rp, err := readpref.New(mode)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidReadPreference, readPreference)
	}
	client, err := mongo.Connect(clientOptions(ctx, uri).SetReadPreference(rp))
	if err != nil {
		// Parse errors may quote parts of the URI; never return them verbatim.
		return nil, errors.New("invalid connection string")
	}
	return &OplogSession{OplogMember: OplogMember{client: client, rp: rp}, uri: uri, tls: mongotls.FromContext(ctx), members: map[string]*mongo.Client{}}, nil
}

// Close disconnects the session's client and the clients of pinned members.
func (s *OplogSession) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	members := s.members
	s.members = map[string]*mongo.Client{}
	s.mu.Unlock()
	for _, c := range members {
		_ = c.Disconnect(ctx)
	}
	_ = s.client.Disconnect(ctx)
}

// Primary returns a reader bound to the primary that reads the oplog with read
// concern majority: the authority the collector asks before it ends a chain,
// since one secondary's view alone never proves a gap or a divergence.
func (s *OplogSession) Primary() *OplogMember {
	return &OplogMember{client: s.client, rp: readpref.Primary(), majority: true}
}

// Pin selects one member through the session's read preference and returns a
// reader bound to it, so a whole collector tick (window, range read and lookups)
// sees one oplog. A member that is not the primary and whose newest write is
// older than notBefore (the collector's position) is stale: the primary is used
// instead. When a direct connection to the member cannot be made the primary is
// used too. Errors are redacted.
func (s *OplogSession) Pin(ctx context.Context, notBefore pitr.Timestamp) (*OplogMember, error) {
	h, err := s.hello(ctx, s.rp)
	if err != nil {
		return nil, err
	}
	stale := !notBefore.IsZero() && pitr.Timestamp{T: h.LastWrite.OpTime.TS.T, I: h.LastWrite.OpTime.TS.I}.Compare(notBefore) < 0
	if h.IsWritablePrimary || stale || h.Me == "" {
		return s.primaryMember(ctx)
	}
	client, err := s.memberClient(ctx, h.Me)
	if err != nil {
		return s.primaryMember(ctx)
	}
	return &OplogMember{client: client, rp: readpref.Nearest(), host: h.Me}, nil
}

// primaryMember returns a reader bound to the primary (read concern local, like
// the members' readers).
func (s *OplogSession) primaryMember(ctx context.Context) (*OplogMember, error) {
	h, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	return &OplogMember{client: s.client, rp: readpref.Primary(), host: h.Me}, nil
}

// oplogHello is the part of hello an oplog session uses.
type oplogHello struct {
	SetName           string `bson:"setName"`
	Me                string `bson:"me"`
	IsWritablePrimary bool   `bson:"isWritablePrimary"`
	LastWrite         struct {
		OpTime struct {
			TS bson.Timestamp `bson:"ts"`
			T  int64          `bson:"t"`
		} `bson:"opTime"`
		MajorityOpTime struct {
			TS bson.Timestamp `bson:"ts"`
			T  int64          `bson:"t"`
		} `bson:"majorityOpTime"`
	} `bson:"lastWrite"`
}

// hello runs hello on the member rp selects.
func (s *OplogSession) hello(ctx context.Context, rp *readpref.ReadPref) (oplogHello, error) {
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	var h oplogHello
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}},
		options.RunCmd().SetReadPreference(rp)).Decode(&h); err != nil {
		return h, redactErr(fmt.Errorf("hello: %w", err))
	}
	if h.SetName == "" {
		return h, pitr.ErrNotReplicaSet
	}
	return h, nil
}

// memberClient returns the cached direct client of host, connecting it once.
func (s *OplogSession) memberClient(ctx context.Context, host string) (*mongo.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.members[host]; ok {
		return c, nil
	}
	opts := clientOptions(mongotls.NewContext(ctx, s.tls), s.uri).SetHosts([]string{host}).SetDirect(true).SetReadPreference(readpref.Nearest())
	opts.SRVMaxHosts, opts.SRVServiceName = nil, nil
	c, err := mongo.Connect(opts)
	if err != nil {
		return nil, errors.New("connect to the replica set member")
	}
	s.members[host] = c
	return c, nil
}

// OplogWindow opens a short-lived session for uri and returns its oplog window
// (see OplogSession.OplogWindow).
func (p *Prober) OplogWindow(ctx context.Context, uri string) (pitr.OplogWindow, error) {
	var w pitr.OplogWindow
	err := p.withOplogSession(ctx, uri, func(s *OplogSession) (err error) {
		w, err = s.OplogWindow(ctx)
		return err
	})
	return w, err
}

// ReadOplog opens a short-lived session for uri and copies the oplog range r to w
// (see OplogSession.ReadOplog).
func (p *Prober) ReadOplog(ctx context.Context, uri string, r pitr.OplogRange, w io.Writer) (pitr.OplogStats, error) {
	var stats pitr.OplogStats
	err := p.withOplogSession(ctx, uri, func(s *OplogSession) (err error) {
		stats, err = s.ReadOplog(ctx, r, w)
		return err
	})
	return stats, err
}

// EntryAt opens a short-lived session for uri and looks up the oplog entry at ts
// (see OplogSession.EntryAt).
func (p *Prober) EntryAt(ctx context.Context, uri string, ts pitr.Timestamp) (term int64, found bool, err error) {
	err = p.withOplogSession(ctx, uri, func(s *OplogSession) (err error) {
		term, found, err = s.EntryAt(ctx, ts)
		return err
	})
	return term, found, err
}

// CanReadOplog opens a short-lived session for uri and reports whether its user
// may read the oplog (see OplogSession.CanReadOplog).
func (p *Prober) CanReadOplog(ctx context.Context, uri string) (bool, error) {
	var ok bool
	err := p.withOplogSession(ctx, uri, func(s *OplogSession) (err error) {
		ok, err = s.CanReadOplog(ctx)
		return err
	})
	return ok, err
}

// withOplogSession runs fn with a session for uri using the default read
// preference and always closes it. Errors are redacted.
func (p *Prober) withOplogSession(ctx context.Context, uri string, fn func(*OplogSession) error) error {
	s, err := p.OpenOplogSession(ctx, uri, "")
	if err != nil {
		return err
	}
	defer s.Close()
	return redactErr(fn(s))
}

// OplogWindow returns the oldest and newest oplog entries of the member the read
// preference selects, the majority-committed optime (hello.lastWrite.majorityOpTime)
// and the replica set name and ID. It returns pitr.ErrNotReplicaSet for a server
// that is not a replica set member. Errors are redacted.
func (s *OplogMember) OplogWindow(ctx context.Context) (pitr.OplogWindow, error) {
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	var w pitr.OplogWindow
	var hello struct {
		SetName   string `bson:"setName"`
		LastWrite struct {
			MajorityOpTime struct {
				TS bson.Timestamp `bson:"ts"`
				T  int64          `bson:"t"`
			} `bson:"majorityOpTime"`
		} `bson:"lastWrite"`
	}
	admin := s.client.Database("admin")
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}, options.RunCmd().SetReadPreference(s.rp)).Decode(&hello); err != nil {
		return w, redactErr(fmt.Errorf("hello: %w", err))
	}
	if hello.SetName == "" {
		return w, pitr.ErrNotReplicaSet
	}
	w.ReplicaSet = hello.SetName
	mt := hello.LastWrite.MajorityOpTime
	w.MajorityOpTime = pitr.OpTime{TS: pitr.Timestamp{T: mt.TS.T, I: mt.TS.I}, Term: mt.T}

	oplog := s.oplog()
	for _, end := range []struct {
		dir int
		dst *pitr.Timestamp
	}{{1, &w.Oldest}, {-1, &w.Newest}} {
		opts := options.FindOne().SetSort(bson.D{{Key: "$natural", Value: end.dir}}).
			SetProjection(bson.D{{Key: "ts", Value: 1}, {Key: "_id", Value: 0}})
		raw, err := oplog.FindOne(ctx, bson.D{}, opts).Raw()
		if err != nil {
			return w, redactErr(fmt.Errorf("read the oplog's ends: %w", err))
		}
		t, i, ok := raw.Lookup("ts").TimestampOK()
		if !ok {
			return w, errors.New("read the oplog's ends: an entry has no ts timestamp")
		}
		*end.dst = pitr.Timestamp{T: t, I: i}
	}

	id, err := s.replicaSetID(ctx)
	if err != nil {
		return w, redactErr(err)
	}
	w.ReplicaSetID = id
	return w, nil
}

// replicaSetID returns the hex settings.replicaSetId of the replica set
// configuration, through replSetGetConfig or else local.system.replset. It is
// empty when the user may read neither.
func (s *OplogMember) replicaSetID(ctx context.Context) (string, error) {
	var cfg struct {
		Config struct {
			Settings struct {
				ID bson.ObjectID `bson:"replicaSetId"`
			} `bson:"settings"`
		} `bson:"config"`
	}
	err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}},
		options.RunCmd().SetReadPreference(s.rp)).Decode(&cfg)
	if err == nil {
		return objectIDHex(cfg.Config.Settings.ID), nil
	}
	if !unauthorized(err) {
		return "", fmt.Errorf("replSetGetConfig: %w", err)
	}
	var doc struct {
		Settings struct {
			ID bson.ObjectID `bson:"replicaSetId"`
		} `bson:"settings"`
	}
	err = s.client.Database(oplogDB).Collection("system.replset").FindOne(ctx, bson.D{},
		options.FindOne().SetProjection(bson.D{{Key: "settings.replicaSetId", Value: 1}})).Decode(&doc)
	switch {
	case err == nil:
		return objectIDHex(doc.Settings.ID), nil
	case unauthorized(err), errors.Is(err, mongo.ErrNoDocuments):
		return "", nil
	default:
		return "", fmt.Errorf("read local.system.replset: %w", err)
	}
}

// objectIDHex renders id in hex, or "" for the zero ID.
func objectIDHex(id bson.ObjectID) string {
	if id.IsZero() {
		return ""
	}
	return id.Hex()
}

// unauthorized reports whether err is the server's missing-privilege error.
func unauthorized(err error) bool {
	var se mongo.ServerError
	return errors.As(err, &se) && se.HasErrorCode(unauthorizedCode)
}

// ErrInvalidRange is returned by ReadOplog for a range that ends before it starts.
var ErrInvalidRange = errors.New("mongoconn: invalid oplog range")

// ReadOplog copies the oplog entries of r to w in $natural order, each as its raw
// BSON document exactly as the server sent it, so memory use is bounded by one
// cursor batch. It queries [r.From, r.To] and proves the range's continuity on the
// member that served it: the first entry must be the one at r.From (with
// r.FromTerm when r.CheckTerm is set), or it returns pitr.ErrOplogGap. That entry
// is written only with r.StartInclusive, so w otherwise receives exactly
// (r.From, r.To]. When the member has no entry at r.To yet, it returns
// pitr.ErrOplogBehind. After an error nothing written may be kept. Errors are
// redacted.
func (s *OplogMember) ReadOplog(ctx context.Context, r pitr.OplogRange, w io.Writer) (pitr.OplogStats, error) {
	if r.To.Compare(r.From) < 0 {
		return pitr.OplogStats{}, fmt.Errorf("%w: it ends at %s before its start %s", ErrInvalidRange, r.To, r.From)
	}
	filter := bson.D{{Key: "ts", Value: bson.D{
		{Key: "$gte", Value: bson.Timestamp{T: r.From.T, I: r.From.I}},
		{Key: "$lte", Value: bson.Timestamp{T: r.To.T, I: r.To.I}},
	}}}
	findCtx, cancel := memberCtx(ctx)
	defer cancel()
	cur, err := s.oplog().Find(findCtx, filter,
		options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
	if err != nil {
		return pitr.OplogStats{}, redactErr(fmt.Errorf("find oplog entries: %w", err))
	}
	defer func() { _ = cur.Close(context.WithoutCancel(ctx)) }()
	stats, err := copyOplog(ctx, driverCursor{cur}, r, w)
	return stats, redactErr(err)
}

// oplogCursor is the part of a driver cursor copyOplog uses, so tests can feed it
// documents without a server.
type oplogCursor interface {
	Next(ctx context.Context) bool
	Doc() bson.Raw
	Err() error
}

// driverCursor adapts *mongo.Cursor to oplogCursor.
type driverCursor struct{ *mongo.Cursor }

// Next advances the cursor; a call that has to fetch the next batch (a getMore)
// is bounded by memberOpTimeout, like every round trip to a member.
func (c driverCursor) Next(ctx context.Context) bool {
	if c.RemainingBatchLength() > 0 {
		return c.Cursor.Next(ctx)
	}
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	return c.Cursor.Next(ctx)
}

// Doc returns the current document.
func (c driverCursor) Doc() bson.Raw { return c.Current }

// copyOplog verifies and copies the result of the query
// {ts: {$gte: r.From, $lte: r.To}} in $natural order (see ReadOplog).
func copyOplog(ctx context.Context, cur oplogCursor, r pitr.OplogRange, w io.Writer) (pitr.OplogStats, error) {
	var (
		stats   pitr.OplogStats
		prev    pitr.Timestamp // the last entry seen, the start entry included
		started bool
	)
	for cur.Next(ctx) {
		doc := cur.Doc()
		t, i, ok := doc.Lookup("ts").TimestampOK()
		if !ok {
			return stats, errors.New("an oplog entry has no ts timestamp")
		}
		op := pitr.OpTime{TS: pitr.Timestamp{T: t, I: i}}
		op.Term, _ = doc.Lookup("t").AsInt64OK()
		switch {
		case !started:
			if op.TS != r.From {
				return stats, fmt.Errorf("%w: the first entry is at %s, not at %s", pitr.ErrOplogGap, op.TS, r.From)
			}
			if r.CheckTerm && op.Term != r.FromTerm {
				return stats, fmt.Errorf("%w: the entry at %s has term %d, not %d", pitr.ErrOplogGap, op.TS, op.Term, r.FromTerm)
			}
			started, prev = true, op.TS
			if !r.StartInclusive {
				continue
			}
		case op.TS.Compare(prev) <= 0:
			return stats, fmt.Errorf("oplog entry %s is not after %s", op.TS, prev)
		}
		if _, err := w.Write(doc); err != nil {
			return stats, fmt.Errorf("write oplog entry: %w", err)
		}
		if stats.Entries == 0 {
			stats.First = op
		}
		stats.Last = op
		stats.Entries++
		prev = op.TS
	}
	if err := cur.Err(); err != nil {
		return stats, fmt.Errorf("read oplog entries: %w", err)
	}
	if !started {
		return stats, fmt.Errorf("%w: no entry at %s", pitr.ErrOplogGap, r.From)
	}
	if prev != r.To {
		return stats, fmt.Errorf("%w: read up to %s of %s", pitr.ErrOplogBehind, prev, r.To)
	}
	return stats, nil
}

// EntryAt looks up the oplog entry at ts and returns its term; found is false when
// the member that answered has no entry there (it was truncated, rolled back or
// never replicated). Errors are redacted.
func (s *OplogMember) EntryAt(ctx context.Context, ts pitr.Timestamp) (term int64, found bool, err error) {
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	raw, err := s.oplog().FindOne(ctx,
		bson.D{{Key: "ts", Value: bson.Timestamp{T: ts.T, I: ts.I}}},
		options.FindOne().SetProjection(bson.D{{Key: "t", Value: 1}, {Key: "_id", Value: 0}})).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, redactErr(fmt.Errorf("find oplog entry %s: %w", ts, err))
	}
	term, _ = raw.Lookup("t").AsInt64OK()
	return term, true, nil
}

// CanReadOplog reports whether the session's user may run find on
// local.oplog.rs, from connectionStatus: a privilege on any resource, on the local
// database or on local.oplog.rs itself (granted by backup, read on local or a
// custom role). readAnyDatabase does not grant it: its database wildcard excludes
// local. A server without access control allows it. Errors are redacted.
func (s *OplogSession) CanReadOplog(ctx context.Context) (bool, error) {
	var res struct {
		AuthInfo struct {
			Users      []bson.Raw  `bson:"authenticatedUsers"`
			Privileges []privilege `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}
	if err := s.client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		return false, redactErr(fmt.Errorf("connectionStatus: %w", err))
	}
	return len(res.AuthInfo.Users) == 0 || grantsOplogFind(res.AuthInfo.Privileges), nil
}

// grantsOplogFind reports whether privs allow find on local.oplog.rs. A database
// wildcard (db "") does not cover local.
func grantsOplogFind(privs []privilege) bool {
	for _, p := range privs {
		r := p.Resource
		covers := r.AnyResource ||
			(r.DB != nil && r.Collection != nil && *r.DB == oplogDB && (*r.Collection == "" || *r.Collection == oplogColl))
		if covers && slices.Contains(p.Actions, "find") {
			return true
		}
	}
	return false
}

// redactedError carries a message scrubbed by redact.Text while still unwrapping to
// the original error, so errors.Is and errors.As keep working.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr returns err with credentials and connection strings in its message
// masked; nil stays nil.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := redact.Text(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}
