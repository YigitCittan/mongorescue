package connections_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// olderReleaseURIs passed the connection string validation of v0.7.0 but not the
// stricter one of later releases.
var olderReleaseURIs = []string{
	"mongodb://u:pa\u00a0ss@h:27017/db", // unencoded no-break space in the password
	"mongodb://u:p\u2028x@h/db",         // line separator
	"mongodb://u:100%@h/db",             // raw '%'
	"mongodb://h{x}/db",                 // host character outside RFC 3986
}

// TestURIsFromOlderReleasesKeepWorking checks that a connection stored (or a
// MONGORESCUE_MONGO_URI imported) by an older release keeps working: it can be
// renamed and re-tested with its redacted URI, and the legacy import accepts it. Only
// newly entered URIs get the stricter validation.
func TestURIsFromOlderReleasesKeepWorking(t *testing.T) {
	ctx := context.Background()
	for i, uri := range olderReleaseURIs {
		svc, st, p := newService(t)
		if _, err := svc.Create(ctx, connections.Input{Name: "new", URI: uri}); !errors.Is(err, connections.ErrInvalid) {
			t.Fatalf("Create(%q) = %v; want ErrInvalid for new input", uri, err)
		}

		// Stored by an older release.
		now := time.Now().UTC()
		id := "conn_legacy" + string(rune('a'+i))
		if err := st.SaveConnection(ctx, &models.Connection{ID: id, Name: "old", URI: uri, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, id, connections.Input{Name: "renamed", URI: redact.URI(uri)}); err != nil {
			t.Errorf("renaming a stored connection with URI %q: %v", uri, err)
		}
		if _, err := svc.Update(ctx, id, connections.Input{Name: "renamed", URI: uri}); err != nil {
			t.Errorf("re-entering the stored URI %q unchanged: %v", uri, err)
		}
		if _, err := svc.TestURI(ctx, redact.URI(uri), id); err != nil {
			t.Errorf("re-testing stored URI %q: %v", uri, err)
		}
		if p.uri() != uri {
			t.Errorf("TestURI probed %q; want the stored URI", p.uri())
		}
		if full, err := svc.Resolve(ctx, id); err != nil || full.URI != uri {
			t.Errorf("Resolve = %v; want the stored URI unchanged", err)
		}

		// Imported from MONGORESCUE_MONGO_URI at startup.
		fresh, _, _ := newService(t)
		if created, err := fresh.EnsureDefault(ctx, uri); !created || err != nil {
			t.Errorf("EnsureDefault(%q) = %v, %v; the legacy import must accept it", uri, created, err)
		}
	}
}
