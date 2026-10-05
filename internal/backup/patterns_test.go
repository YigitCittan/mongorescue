package backup

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// patternEngine returns an engine whose database lists collections and whose
// manifest has one entry per listed collection.
func patternEngine(runner *argsRunner, collections ...string) *Engine {
	lister := func(context.Context, string, string) ([]string, error) { return slices.Clone(collections), nil }
	manifest := func(context.Context, string, string) (*models.Manifest, error) {
		m := &models.Manifest{}
		for _, c := range collections {
			m.Collections = append(m.Collections, models.CollectionManifest{Name: c})
		}
		return m, nil
	}
	return NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run),
		WithCollectionLister(lister), WithManifestCapturer(manifest))
}

// manifestNames returns the collections of rec's manifest, sorted.
func manifestNames(rec *models.BackupRecord) []string {
	if rec.Manifest == nil {
		return nil
	}
	var out []string
	for _, c := range rec.Manifest.Collections {
		out = append(out, c.Name)
	}
	slices.Sort(out)
	return out
}

func TestExcludePatternsAreExpanded(t *testing.T) {
	for name, tc := range map[string]struct {
		collections []string
		exclude     []string
		want        []string
		manifest    []string
	}{
		"matching none": {
			collections: []string{"orders", "logs"}, exclude: []string{"tmp_*"},
			want: nil, manifest: []string{"logs", "orders"},
		},
		"matching one": {
			collections: []string{"orders", "tmp_a"}, exclude: []string{"tmp_*"},
			want: []string{"tmp_a"}, manifest: []string{"orders"},
		},
		"matching many, with a literal name and ?": {
			collections: []string{"orders", "tmp_a", "tmp_bb", "logs", "log1", "system.tmp_x"},
			exclude:     []string{"tmp_*", "log?", "logs", "tmp_a"},
			want:        []string{"tmp_a", "tmp_bb", "logs", "log1"}, manifest: []string{"orders", "system.tmp_x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &argsRunner{}
			rec, err := patternEngine(runner, tc.collections...).Run(context.Background(), models.BackupOptions{Database: "shop", ExcludeCollections: tc.exclude})
			if err != nil {
				t.Fatal(err)
			}
			if got := runner.values("--excludeCollection"); !slices.Equal(got, tc.want) {
				t.Errorf("--excludeCollection = %q; want %q", got, tc.want)
			}
			// No pattern ever reaches mongodump as a name.
			for _, a := range runner.args {
				if models.IsCollectionPattern(a) && a != "--archive" {
					t.Errorf("mongodump argument %q is a pattern", a)
				}
			}
			if !slices.Equal(rec.ExcludedCollections, tc.want) || rec.Collections != nil {
				t.Errorf("record filter = %q, %q; want exclusions %q", rec.Collections, rec.ExcludedCollections, tc.want)
			}
			if got := manifestNames(rec); !slices.Equal(got, tc.manifest) {
				t.Errorf("manifest = %q; want %q", got, tc.manifest)
			}
		})
	}
}

func TestIncludePatternsAreExpanded(t *testing.T) {
	collections := []string{"orders", "orders_old", "customers", "logs", "system.views"}
	runner := &argsRunner{}
	rec, err := patternEngine(runner, collections...).Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"ord*"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.values("--collection"); len(got) != 0 {
		t.Errorf("--collection = %q; several matches become exclusions", got)
	}
	if got := runner.values("--excludeCollection"); !slices.Equal(got, []string{"customers", "logs"}) {
		t.Errorf("--excludeCollection = %q", got)
	}
	if !slices.Equal(rec.Collections, []string{"orders", "orders_old"}) || rec.ExcludedCollections != nil {
		t.Errorf("record filter = %q, %q", rec.Collections, rec.ExcludedCollections)
	}
	if got := manifestNames(rec); !slices.Equal(got, []string{"orders", "orders_old", "system.views"}) {
		t.Errorf("manifest = %q", got)
	}

	// One match is passed as --collection.
	runner = &argsRunner{}
	rec, err = patternEngine(runner, collections...).Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"cust?mers"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.values("--collection"); !slices.Equal(got, []string{"customers"}) {
		t.Errorf("--collection = %q", got)
	}
	if !slices.Equal(rec.Collections, []string{"customers"}) {
		t.Errorf("record collections = %q", rec.Collections)
	}

	// An include pattern with an exclude pattern: what the include matches, less the
	// exclusions.
	runner = &argsRunner{}
	rec, err = patternEngine(runner, collections...).Run(context.Background(), models.BackupOptions{
		Database: "shop", Collections: []string{"*"}, ExcludeCollections: []string{"*_old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.values("--excludeCollection"); !slices.Equal(got, []string{"orders_old"}) {
		t.Errorf("--excludeCollection = %q", got)
	}
	if !slices.Equal(rec.Collections, []string{"orders", "orders_old", "customers", "logs"}) || !slices.Equal(rec.ExcludedCollections, []string{"orders_old"}) {
		t.Errorf("record filter = %q, %q", rec.Collections, rec.ExcludedCollections)
	}
}

func TestIncludePatternMatchingNothingFails(t *testing.T) {
	runner := &argsRunner{}
	rec, err := patternEngine(runner, "orders").Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"orders", "tmp_*"}})
	if !errors.Is(err, ErrCollectionFilter) {
		t.Fatalf("err = %v; want ErrCollectionFilter", err)
	}
	if runner.called || rec.Status != models.StatusFailed {
		t.Fatalf("mongodump must not run: called=%v status=%s", runner.called, rec.Status)
	}
}

func TestPatternsWithoutListerAreRefused(t *testing.T) {
	runner := &argsRunner{}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner.run))
	for _, opts := range []models.BackupOptions{
		{Database: "shop", ExcludeCollections: []string{"tmp_*"}},
		{Database: "shop", Collections: []string{"ord?rs"}},
	} {
		// Refused before the backup starts, so the API answers 400.
		if _, err := engine.Prepare(opts); !errors.Is(err, ErrCollectionFilter) {
			t.Errorf("Prepare(%+v) = %v; want ErrCollectionFilter", opts, err)
		}
		if _, err := engine.Run(context.Background(), opts); !errors.Is(err, ErrCollectionFilter) {
			t.Errorf("Run(%+v) = %v; want ErrCollectionFilter", opts, err)
		}
	}
	if runner.called {
		t.Error("mongodump ran with an unexpanded pattern")
	}
	if engine.ListsCollections() || !patternEngine(runner).ListsCollections() {
		t.Error("ListsCollections does not report the lister")
	}
}
