package mongoconn

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// WriteOpTimes opens a short-lived session for uri and returns
// hello.lastWrite.opTime and hello.lastWrite.majorityOpTime of the primary: the
// newest write and the newest majority-committed one. Base backups record T_before
// from the first and T_after from the second. It returns pitr.ErrNotReplicaSet
// for a server that is not a replica set member. Errors are redacted.
func (p *Prober) WriteOpTimes(ctx context.Context, uri string) (lastWrite, majority pitr.OpTime, err error) {
	s, err := p.OpenOplogSession(ctx, uri, readpref.PrimaryMode.String())
	if err != nil {
		return pitr.OpTime{}, pitr.OpTime{}, err
	}
	defer s.Close()
	h, err := s.hello(ctx, readpref.Primary())
	if err != nil {
		return pitr.OpTime{}, pitr.OpTime{}, err
	}
	lw, mj := h.LastWrite.OpTime, h.LastWrite.MajorityOpTime
	return pitr.OpTime{TS: pitr.Timestamp{T: lw.TS.T, I: lw.TS.I}, Term: lw.T},
		pitr.OpTime{TS: pitr.Timestamp{T: mj.TS.T, I: mj.TS.I}, Term: mj.T}, nil
}

// LastWrite returns hello.lastWrite.opTime of the member the session's read
// preference selects. It returns pitr.ErrNotReplicaSet for a server that is not a
// replica set member. Errors are redacted.
func (s *OplogMember) LastWrite(ctx context.Context) (pitr.OpTime, error) {
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	var hello struct {
		SetName   string `bson:"setName"`
		LastWrite struct {
			OpTime struct {
				TS bson.Timestamp `bson:"ts"`
				T  int64          `bson:"t"`
			} `bson:"opTime"`
		} `bson:"lastWrite"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}},
		options.RunCmd().SetReadPreference(s.rp)).Decode(&hello); err != nil {
		return pitr.OpTime{}, redactErr(fmt.Errorf("hello: %w", err))
	}
	if hello.SetName == "" {
		return pitr.OpTime{}, pitr.ErrNotReplicaSet
	}
	op := hello.LastWrite.OpTime
	return pitr.OpTime{TS: pitr.Timestamp{T: op.TS.T, I: op.TS.I}, Term: op.T}, nil
}

// EntryAtOrAfter returns the position of the first oplog entry at or after ts on
// the member that answered; found is false when there is none. The query starts
// at ts, which the oplog's ts lookup serves without reading older entries. The
// collector uses it to end a catch-up chunk after one interval. Errors are
// redacted.
func (s *OplogMember) EntryAtOrAfter(ctx context.Context, ts pitr.Timestamp) (op pitr.OpTime, found bool, err error) {
	ctx, cancel := memberCtx(ctx)
	defer cancel()
	raw, err := s.oplog().FindOne(ctx,
		bson.D{{Key: "ts", Value: bson.D{{Key: "$gte", Value: bson.Timestamp{T: ts.T, I: ts.I}}}}},
		options.FindOne().SetSort(bson.D{{Key: "$natural", Value: 1}}).
			SetProjection(bson.D{{Key: "ts", Value: 1}, {Key: "t", Value: 1}, {Key: "_id", Value: 0}})).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return pitr.OpTime{}, false, nil
	}
	if err != nil {
		return pitr.OpTime{}, false, redactErr(fmt.Errorf("find the oplog entry at or after %s: %w", ts, err))
	}
	t, i, ok := raw.Lookup("ts").TimestampOK()
	if !ok {
		return pitr.OpTime{}, false, errors.New("an oplog entry has no ts timestamp")
	}
	op.TS = pitr.Timestamp{T: t, I: i}
	op.Term = entryTerm(raw)
	return op, true, nil
}
