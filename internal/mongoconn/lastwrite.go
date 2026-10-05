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

// LastWrite opens a short-lived session for uri and returns
// hello.lastWrite.opTime of the primary: the position of the newest write. Base
// backups record it before and after the dump (T_before, T_after). It returns
// pitr.ErrNotReplicaSet for a server that is not a replica set member. Errors are
// redacted.
func (p *Prober) LastWrite(ctx context.Context, uri string) (pitr.OpTime, error) {
	s, err := p.OpenOplogSession(ctx, uri, readpref.PrimaryMode.String())
	if err != nil {
		return pitr.OpTime{}, err
	}
	defer s.Close()
	op, err := s.LastWrite(ctx)
	return op, redactErr(err)
}

// LastWrite returns hello.lastWrite.opTime of the member the session's read
// preference selects. It returns pitr.ErrNotReplicaSet for a server that is not a
// replica set member. Errors are redacted.
func (s *OplogMember) LastWrite(ctx context.Context) (pitr.OpTime, error) {
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
	op.Term, _ = raw.Lookup("t").AsInt64OK()
	return op, true, nil
}
