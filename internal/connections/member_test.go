package connections_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// memberProber also reports the member a read selects.
type memberProber struct {
	prober
	mu        sync.Mutex
	memberURI string
	err       error
}

func (m *memberProber) ServingMember(_ context.Context, uri string) (models.SourceMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memberURI = uri
	if m.err != nil {
		return models.SourceMember{}, m.err
	}
	return models.SourceMember{Host: "db2:27017", State: models.MemberSecondary, SetName: "rs0"}, nil
}

func TestConnectionTestReportsTheReadMember(t *testing.T) {
	st := storetest.New(t)
	p := &memberProber{}
	svc := connections.NewService(st, p)
	ctx := context.Background()
	mode := models.ReadSecondary
	limit := 3
	c, err := svc.Create(ctx, connections.Input{Name: "rs", URI: "mongodb://u:topsecret@db1,db2/?replicaSet=rs0",
		ReadPreference: &mode, ReadPreferenceTags: []map[string]string{{"dc": "east"}}, MaxConcurrentBackups: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadPreference != mode || c.MaxConcurrentBackups != 3 {
		t.Fatalf("created %+v", c)
	}
	res, err := svc.Test(ctx, c.ID)
	if err != nil || !res.OK {
		t.Fatalf("test %+v, %v", res, err)
	}
	if res.ReadMember == nil || res.ReadMember.Host != "db2:27017" || res.ReadPreference != "secondary (dc=east)" {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(p.memberURI, "readPreference=secondary&readPreferenceTags=dc:east") {
		t.Fatalf("member probed with %q", p.memberURI)
	}

	p.err = errors.New("server selection error for mongodb://u:topsecret@db1")
	res, _ = svc.TestURIWith(ctx, "mongodb://u:topsecret@db1,db2/?replicaSet=rs0", "", models.ReadPreference{Mode: models.ReadSecondary})
	if !res.OK || res.ReadMember != nil || res.ReadMemberError == "" || strings.Contains(res.ReadMemberError, "topsecret") {
		t.Fatalf("result %+v", res)
	}
	if _, err = svc.TestURIWith(ctx, "mongodb://db1", "", models.ReadPreference{Mode: "fastest"}); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("invalid read preference: %v", err)
	}
}

func TestPrimaryRefusesMaxStaleness(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &prober{})
	ctx := context.Background()
	primary, secondary := models.ReadPrimary, models.ReadSecondary
	const uri = "mongodb://db1,db2/?replicaSet=rs0&maxStalenessSeconds=120"
	_, err := svc.Create(ctx, connections.Input{Name: "rs", URI: uri, ReadPreference: &primary})
	if !errors.Is(err, connections.ErrInvalid) || !strings.Contains(err.Error(), "maxStalenessSeconds") {
		t.Fatalf("primary with maxStalenessSeconds: %v", err)
	}
	c, err := svc.Create(ctx, connections.Input{Name: "rs", URI: uri, ReadPreference: &secondary})
	if err != nil {
		t.Fatalf("secondary with maxStalenessSeconds: %v", err)
	}
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "rs", URI: c.URI, ReadPreference: &primary}); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("update to primary with maxStalenessSeconds: %v", err)
	}
}
