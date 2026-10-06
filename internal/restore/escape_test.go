package restore

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestEscapeNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"orders":    "orders",
		"a*":        `a\*`,
		"*":         `\*`,
		`a\b`:       `a\\b`,
		`a\*`:       `a\\\*`,
		`\\**`:      `\\\\\*\*`,
		"x*_rescue": `x\*_rescue`,
	} {
		if got := escapeNamespace(in); got != want {
			t.Errorf("escapeNamespace(%q) = %q; want %q", in, got, want)
		}
	}
}

// nsArgs returns the namespace arguments of args.
func nsArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "--ns") {
			out = append(out, a)
		}
	}
	return out
}

// TestRestoreArgsEscapeWildcards checks the namespace arguments for names holding
// mongorestore's wildcard and escape characters, in safe-clone and in-place mode:
// a selected "a*" must match the collection "a*" only (with --drop it would otherwise
// drop every collection starting with "a"), and a database "x*" only itself.
func TestRestoreArgsEscapeWildcards(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "mongodb://h")
	no := false
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	clone, err := models.RescueDatabaseName("x*", at, "ab12")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		source, target string
		inPlace        bool
		selected       []string
		want           []string
	}{
		{"safe clone, selected a*", "shop", "shop_rescue_20260925_120000", false, []string{"a*"},
			[]string{"--nsFrom=shop.*", "--nsTo=shop_rescue_20260925_120000.*", `--nsInclude=shop.a\*`}},
		{"in place, selected a*, a\\b and *", "shop", "shop", true, []string{"a*", `a\b`, "*"},
			[]string{`--nsInclude=shop.a\*`, `--nsInclude=shop.a\\b`, `--nsInclude=shop.\*`}},
		{"safe clone of database x*", "x*", clone, false, nil,
			[]string{`--nsFrom=x\*.*`, `--nsTo=x\*_rescue_20260925_120000_ab12.*`, `--nsInclude=x\*.*`}},
		{"safe clone of database x*, selected a*", "x*", clone, false, []string{"a*"},
			[]string{`--nsFrom=x\*.*`, `--nsTo=x\*_rescue_20260925_120000_ab12.*`, `--nsInclude=x\*.a\*`}},
		{"in place into database x*, selected a*", "x*", "x*", true, []string{"a*"},
			[]string{`--nsInclude=x\*.a\*`}},
		{"in place from x* into y*", "x*", "y*", true, nil,
			[]string{`--nsFrom=x\*.*`, `--nsTo=y\*.*`, `--nsInclude=x\*.*`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := models.RestoreRequest{BackupID: "bkp_1", DropTarget: true, SelectedCollections: tc.selected}
			if tc.inPlace {
				req.SafeClone, req.ConfirmInPlace = &no, true
			}
			args := e.buildRestoreArgs("--config=x", tc.source, tc.target, req, false)
			if got := nsArgs(args); !slices.Equal(got, tc.want) {
				t.Fatalf("namespace args = %q\nwant             %q", got, tc.want)
			}
			if !slices.Contains(args, "--drop") {
				t.Fatalf("args %q lack --drop", args)
			}
		})
	}
}
