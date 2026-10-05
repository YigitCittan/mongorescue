package mongoconn

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestMemberOf(t *testing.T) {
	cases := []struct {
		reply helloReply
		want  models.SourceMember
	}{
		{helloReply{Me: "db1:27017", SetName: "rs0", IsWritablePrimary: true}, models.SourceMember{Host: "db1:27017", SetName: "rs0", State: models.MemberPrimary}},
		{helloReply{Me: "db1:27017", SetName: "rs0", IsMaster: true}, models.SourceMember{Host: "db1:27017", SetName: "rs0", State: models.MemberPrimary}},
		{helloReply{Me: "db2:27017", SetName: "rs0", Secondary: true}, models.SourceMember{Host: "db2:27017", SetName: "rs0", State: models.MemberSecondary}},
		{helloReply{Me: "db3:27017", SetName: "rs0"}, models.SourceMember{Host: "db3:27017", SetName: "rs0", State: models.MemberOther}},
		{helloReply{IsWritablePrimary: true}, models.SourceMember{State: models.MemberStandalone}},
		{helloReply{Msg: "isdbgrid", IsWritablePrimary: true}, models.SourceMember{State: models.MemberMongos}},
	}
	for _, tc := range cases {
		if got := memberOf(tc.reply); got != tc.want {
			t.Errorf("memberOf(%+v) = %+v, want %+v", tc.reply, got, tc.want)
		}
	}
}

func TestReadPreferenceOf(t *testing.T) {
	ctx := context.Background()
	if rp := readPreferenceOf(ctx, "mongodb://db1/"); rp.Mode() != readpref.PrimaryMode {
		t.Fatalf("default %v", rp.Mode())
	}
	rp := readPreferenceOf(ctx, "mongodb://db1/?readPreference=secondary&readPreferenceTags=dc:east")
	if rp.Mode() != readpref.SecondaryMode || len(rp.TagSets()) != 1 {
		t.Fatalf("parsed %v %v", rp.Mode(), rp.TagSets())
	}
}
