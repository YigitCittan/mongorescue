package restore

import (
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// FuzzPrepareNamespaces checks that Prepare never panics and never refuses a backup
// for its database name (only client input is validated, by the operations layer),
// that a restore goes into a "<db>_rescue_<timestamp>" clone of at most 63 bytes
// unless it is confirmed in place, and that each name reaches mongorestore only inside
// its own --nsFrom/--nsTo/--nsInclude element, with '*' and backslashes escaped.
func FuzzPrepareNamespaces(f *testing.F) {
	f.Add("shop", "", "users", false)
	f.Add("ecommerce_prod", "", "", false)
	f.Add(strings.Repeat("d", 80), "", "", false)
	f.Add(strings.Repeat("d", 50), "", "orders", false)
	f.Add("shop", "shop_copy", "a*b", true)
	f.Add("shop", "--dir=x", "--drop", true)
	f.Add("legacy db", "", "x --dir=/", false)
	f.Add("a*b", "", `c\d`, false)
	f.Add("shop", "", "orders.archive", false)
	f.Fuzz(func(t *testing.T, sourceDB, targetDB, collection string, inPlace bool) {
		e := NewEngine(storage.NewMockStorage(), "mongodb://h")
		req := models.RestoreRequest{BackupID: "bkp_1", TargetDatabase: targetDB, SelectedCollections: []string{collection}}
		if inPlace {
			no := false
			req.SafeClone, req.ConfirmInPlace = &no, true
		}
		src := &models.BackupRecord{ID: "bkp_1", Database: sourceDB}
		rec, err := e.Prepare(req, src)
		if err != nil {
			if req.ValidateTarget() == nil {
				t.Fatalf("Prepare refused a confirmed request for database %q: %v", sourceDB, err)
			}
			return
		}
		target := rec.TargetDatabase
		switch {
		case !req.InPlace():
			if !strings.Contains(target, "_rescue_") {
				t.Fatalf("safe-clone restore of %q targets %q", sourceDB, target)
			}
			if len(target) > models.MaxDatabaseNameLength {
				t.Fatalf("clone name %q is longer than %d bytes", target, models.MaxDatabaseNameLength)
			}
		case strings.TrimSpace(targetDB) != "":
			if target != strings.TrimSpace(targetDB) {
				t.Fatalf("in-place restore into %q targets %q", targetDB, target)
			}
		default:
			if target != sourceDB {
				t.Fatalf("in-place restore of %q targets %q", sourceDB, target)
			}
		}

		args := e.buildRestoreArgs("--config=x", sourceDB, target, req, false)
		// Backslashes and '*' in names are escaped; only a trailing ".*" is a wildcard.
		esc := strings.NewReplacer(`\`, `\\`, `*`, `\*`).Replace
		srcNS := esc(sourceDB)
		var want []string
		if target != "" && target != sourceDB {
			want = append(want, "--nsFrom="+srcNS+".*", "--nsTo="+esc(target)+".*")
		}
		// A blank selected collection is skipped: the whole database is restored.
		if coll := strings.TrimSpace(collection); coll != "" {
			want = append(want, "--nsInclude="+srcNS+"."+esc(coll))
		} else if sourceDB != "" {
			want = append(want, "--nsInclude="+srcNS+".*")
		}
		var got []string
		for _, a := range args {
			if strings.HasPrefix(a, "--ns") {
				got = append(got, a)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("namespace arguments = %q; want %q", got, want)
		}
	})
}

func TestPrepareShortensLongCloneNames(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "mongodb://h")
	rec, err := e.Prepare(models.RestoreRequest{BackupID: "bkp_1"}, &models.BackupRecord{ID: "bkp_1", Database: strings.Repeat("d", 50)})
	if err != nil || len(rec.TargetDatabase) > models.MaxDatabaseNameLength || !strings.HasPrefix(rec.TargetDatabase, strings.Repeat("d", 40)+"_rescue_") {
		t.Fatalf("clone of a 50-byte database = %q, %v", rec.TargetDatabase, err)
	}
}
