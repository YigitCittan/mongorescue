package mongoconn

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// commandNotFound is the server error code of an unknown command (hello before
// MongoDB 4.4.2).
const commandNotFound = 59

// helloReply is the part of a hello (or isMaster) reply ServingMember reads.
type helloReply struct {
	Me                string `bson:"me"`
	SetName           string `bson:"setName"`
	IsWritablePrimary bool   `bson:"isWritablePrimary"`
	IsMaster          bool   `bson:"ismaster"`
	Secondary         bool   `bson:"secondary"`
	Msg               string `bson:"msg"`
}

// readPreferenceOf returns the read preference uri sets, or the primary.
func readPreferenceOf(ctx context.Context, uri string) *readpref.ReadPref {
	if rp := clientOptions(ctx, uri).ReadPreference; rp != nil {
		return rp
	}
	return readpref.Primary()
}

// ServingMember returns the member a read with uri's read preference (its
// readPreference and readPreferenceTags options) selects: hello runs on that
// member and reports its host, its state and its replica set. With several
// eligible members mongodump may pick another one of them; it is still a member
// that satisfies the same read preference. Errors never quote the URI.
func (p *Prober) ServingMember(ctx context.Context, uri string) (models.SourceMember, error) {
	rp := readPreferenceOf(ctx, uri)
	var member models.SourceMember
	err := withClient(ctx, uri, func(c *mongo.Client) error {
		admin := c.Database("admin")
		run := options.RunCmd().SetReadPreference(rp)
		var reply helloReply
		err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}, run).Decode(&reply)
		var cmdErr mongo.CommandError
		if errors.As(err, &cmdErr) && cmdErr.Code == commandNotFound {
			err = admin.RunCommand(ctx, bson.D{{Key: "isMaster", Value: 1}}, run).Decode(&reply)
		}
		if err != nil {
			return fmt.Errorf("hello: %w", err)
		}
		member = memberOf(reply)
		return nil
	})
	return member, err
}

// memberOf classifies a hello reply.
func memberOf(r helloReply) models.SourceMember {
	m := models.SourceMember{Host: r.Me, SetName: r.SetName}
	switch {
	case r.Msg == "isdbgrid":
		m.State = models.MemberMongos
	case r.SetName == "":
		m.State = models.MemberStandalone
	case r.IsWritablePrimary || r.IsMaster:
		m.State = models.MemberPrimary
	case r.Secondary:
		m.State = models.MemberSecondary
	default:
		m.State = models.MemberOther
	}
	return m
}
