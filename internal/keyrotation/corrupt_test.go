package keyrotation_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// TestCorruptSecretDoesNotBlockTheRotation damages one sealed value (sealed with
// another key, as after a bad restore or tampering): the rotation goes on, leaves
// it as it was and names it, and every other secret is re-sealed.
func TestCorruptSecretDoesNotBlockTheRotation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	now := time.Now().UTC()
	if err := opened.Store.SaveConnection(ctx, &models.Connection{ID: "conn_bad", Name: "bad", URI: "mongodb://x/", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	otherKey, _ := secretbox.GenerateKey()
	other, _ := secretbox.New(otherKey)
	planted, _ := other.Seal(secretbox.At("connections", "conn_bad", "uri"), "mongodb://planted/")
	db, err := sql.Open("sqlite", e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE connections SET data = json_set(data, '$.uri', ?) WHERE id = 'conn_bad'", planted); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res, err := rotator(e, opened, nil, nil).Rotate(ctx)
	if err != nil {
		t.Fatalf("rotate with one damaged secret: %v", err)
	}
	if !slices.Equal(res.Skipped, []string{"connections|conn_bad|uri"}) {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	checkSecrets(t, opened.Store)
	_ = opened.Store.Close()
	again := e.start(t)
	checkSecrets(t, again.Store)
}
