package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestConnectionAllowMongos proves that allow_mongos defaults to false, is stored
// and returned by the API, and is kept by an update that omits it.
func TestConnectionAllowMongos(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	rec := serve(h, "POST", "/api/v1/connections", []byte(`{"name":"plain","uri":"mongodb://db1:27017"}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	var c models.Connection
	decodeData(t, rec, &c)
	if c.AllowMongos {
		t.Fatalf("allow_mongos defaults to true: %+v", c)
	}
	if rec = serve(h, "PUT", "/api/v1/connections/"+c.ID, []byte(`{"name":"plain","uri":"`+c.URI+`","allow_mongos":true}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	var updated models.Connection
	decodeData(t, rec, &updated)
	if !updated.AllowMongos {
		t.Fatalf("updated %+v", updated)
	}
	if rec = serve(h, "PUT", "/api/v1/connections/"+c.ID, []byte(`{"name":"renamed","uri":"`+c.URI+`"}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	repo, ok := metaStore.(interface {
		GetConnection(context.Context, string) (*models.Connection, error)
	})
	if !ok {
		t.Fatal("the test store holds no connections")
	}
	stored, err := repo.GetConnection(context.Background(), c.ID)
	if err != nil || stored.Name != "renamed" || !stored.AllowMongos {
		t.Fatalf("stored %+v, %v", stored, err)
	}
}
