package models

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMatchDatabasePattern(t *testing.T) {
	for _, tt := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"prod_*", "prod_", true},
		{"prod_*", "prod_eu", true},
		{"prod_*", "Prod_eu", false}, // case-sensitive
		{"prod_*", "xprod_eu", false},
		{"*_eu", "prod_eu", true},
		{"*_eu", "prod_us", false},
		{"db?", "db1", true},
		{"db?", "db", false},
		{"db?", "db12", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"[x]", "[x]", true}, // only * and ? are wildcards
		{"[x]", "x", false},
		{"ş*", "şirket", true}, // characters, not bytes
		{"?irket", "şirket", true},
		{"**", "x", true},
	} {
		if got := MatchDatabasePattern(tt.pattern, tt.name); got != tt.want {
			t.Errorf("MatchDatabasePattern(%q, %q) = %v; want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestValidateDatabasePattern(t *testing.T) {
	for _, ok := range []string{"*", "prod_*", "db?", "a*b", "[x]"} {
		if err := ValidateDatabasePattern(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-*", "a.b*", "a b", "a/b", "a$", "a\x00", strings.Repeat("x", 64)} {
		if err := ValidateDatabasePattern(bad); !errors.Is(err, ErrInvalidNamespace) {
			t.Errorf("%q: %v; want ErrInvalidNamespace", bad, err)
		}
	}
}

func TestNormalizeSelection(t *testing.T) {
	valid := []DatabaseSelection{
		{Mode: SelectionSingle, Databases: []string{"shop"}},
		{Mode: SelectionSingle, Databases: []string{"admin"}}, // single jobs keep backing up what they named
		{Mode: SelectionList, Databases: []string{" a ", "b", "a"}},
		{Mode: SelectionAll},
		{Mode: SelectionAll, Exclude: []string{"tmp_*"}, AutoIncludeNew: true},
		{Mode: SelectionPattern, Include: []string{"prod_*"}, Exclude: []string{"prod_tmp"}, Databases: []string{"billing"}},
	}
	for _, sel := range valid {
		if err := sel.Normalize(); err != nil {
			t.Errorf("%+v: %v", sel, err)
		}
	}
	list := DatabaseSelection{Mode: SelectionList, Databases: []string{" a ", "b", "a", ""}}
	_ = list.Normalize()
	if !slices.Equal(list.Databases, []string{"a", "b"}) {
		t.Errorf("normalized databases = %q; want trimmed and de-duplicated", list.Databases)
	}

	invalid := []DatabaseSelection{
		{},
		{Mode: "some"},
		{Mode: SelectionSingle},
		{Mode: SelectionSingle, Databases: []string{"a", "b"}},
		{Mode: SelectionSingle, Databases: []string{"a"}, Exclude: []string{"x"}},
		{Mode: SelectionList},
		{Mode: SelectionList, Databases: []string{"admin"}},
		{Mode: SelectionList, Databases: []string{"a"}, AutoIncludeNew: true},
		{Mode: SelectionList, Databases: []string{"a.b"}},
		{Mode: SelectionAll, Include: []string{"x*"}},
		{Mode: SelectionAll, Databases: []string{"local"}},
		{Mode: SelectionPattern},
		{Mode: SelectionPattern, Include: []string{"bad name*"}},
		{Mode: SelectionPattern, Include: []string{"x*"}, Exclude: []string{"-x"}},
	}
	for _, sel := range invalid {
		if err := sel.Normalize(); !errors.Is(err, ErrInvalidSelection) {
			t.Errorf("%+v: %v; want ErrInvalidSelection", sel, err)
		}
	}
}

func excludedReasons(res DatabaseResolution) map[string]string {
	out := map[string]string{}
	for _, e := range res.Excluded {
		out[e.Name] = e.Reason
	}
	return out
}

func TestResolveSelectionModes(t *testing.T) {
	server := []string{"admin", "config", "local", "prod_a", "prod_b", "prod_tmp1", "dev"}

	single := ResolveSelection(DatabaseSelection{Mode: SelectionSingle, Databases: []string{"gone"}}, server, nil)
	if !slices.Equal(single.Included, []string{"gone"}) || len(single.Missing) != 0 {
		t.Errorf("single = %+v; a single job backs up its database without checking", single)
	}

	list := ResolveSelection(DatabaseSelection{Mode: SelectionList, Databases: []string{"dev", "gone"}}, server, nil)
	reasons := excludedReasons(list)
	if !slices.Equal(list.Included, []string{"dev"}) || !slices.Equal(list.Missing, []string{"gone"}) ||
		reasons["gone"] != ExcludedNotFound || reasons["prod_a"] != ExcludedNotSelected || reasons["admin"] != ExcludedSystem {
		t.Errorf("list = %+v", list)
	}

	all := ResolveSelection(DatabaseSelection{Mode: SelectionAll, Exclude: []string{"prod_tmp?"}}, server, nil)
	reasons = excludedReasons(all)
	if !slices.Equal(all.Included, []string{"dev", "prod_a", "prod_b"}) ||
		reasons["admin"] != ExcludedSystem || reasons["config"] != ExcludedSystem || reasons["local"] != ExcludedSystem ||
		reasons["prod_tmp1"] != ExcludedByPattern {
		t.Errorf("all = %+v", all)
	}
	if !slices.Equal(all.Known, all.Included) || len(all.New) != 0 {
		t.Errorf("a first resolution knows everything it matched: known %v, new %v", all.Known, all.New)
	}

	pattern := ResolveSelection(DatabaseSelection{Mode: SelectionPattern, Include: []string{"prod_*", "nomatch_*"},
		Exclude: []string{"prod_tmp*"}, Databases: []string{"dev"}}, server, nil)
	reasons = excludedReasons(pattern)
	if !slices.Equal(pattern.Included, []string{"dev", "prod_a", "prod_b"}) || reasons["prod_tmp1"] != ExcludedByPattern ||
		reasons["admin"] != ExcludedSystem {
		t.Errorf("pattern = %+v", pattern)
	}
	if len(pattern.Warnings) != 1 || !strings.Contains(pattern.Warnings[0], "nomatch_*") {
		t.Errorf("warnings = %v; want one for the pattern that matches nothing", pattern.Warnings)
	}
	// Even "*" never matches a system database.
	star := ResolveSelection(DatabaseSelection{Mode: SelectionPattern, Include: []string{"*"}}, server, nil)
	if slices.ContainsFunc(star.Included, IsSystemDatabase) {
		t.Errorf("* included a system database: %v", star.Included)
	}
}

func TestResolveSelectionKnownDatabases(t *testing.T) {
	server := []string{"prod_a", "prod_b", "prod_c"}
	sel := DatabaseSelection{Mode: SelectionPattern, Include: []string{"prod_*"}}
	known := []string{"prod_a", "prod_b"}

	frozen := ResolveSelection(sel, server, known)
	if !slices.Equal(frozen.Included, []string{"prod_a", "prod_b"}) || !slices.Equal(frozen.New, []string{"prod_c"}) ||
		excludedReasons(frozen)["prod_c"] != ExcludedNew || !slices.Equal(frozen.Known, known) {
		t.Errorf("without auto_include_new = %+v", frozen)
	}

	sel.AutoIncludeNew = true
	auto := ResolveSelection(sel, server, known)
	if !slices.Equal(auto.Included, server) || !slices.Equal(auto.New, []string{"prod_c"}) || !slices.Equal(auto.Known, server) {
		t.Errorf("with auto_include_new = %+v", auto)
	}

	// An empty known set (nothing matched when saved) is still a known set.
	empty := ResolveSelection(DatabaseSelection{Mode: SelectionAll}, server, []string{})
	if len(empty.Included) != 0 || len(empty.New) != 3 {
		t.Errorf("empty known set = %+v", empty)
	}
	if len(empty.Warnings) == 0 {
		t.Error("a selection that matches nothing must warn")
	}
}

func TestJobSelectionCompat(t *testing.T) {
	legacy := &Job{Database: "shop"}
	if sel := legacy.Selection(); sel.Mode != SelectionSingle || !slices.Equal(sel.Databases, []string{"shop"}) || legacy.MultiDatabase() {
		t.Errorf("a job without a selection = %+v", sel)
	}
	multi := &Job{DatabaseSelection: DatabaseSelection{Mode: SelectionAll}, Parallelism: 9}
	if !multi.MultiDatabase() || multi.EffectiveParallelism() != MaxJobParallelism {
		t.Errorf("multi = %v, parallelism %d", multi.MultiDatabase(), multi.EffectiveParallelism())
	}
	clone := multi.Clone()
	clone.DatabaseSelection.Exclude = append(clone.DatabaseSelection.Exclude, "x")
	if len(multi.DatabaseSelection.Exclude) != 0 {
		t.Error("Clone shares the selection")
	}
}

func TestJobRunFinish(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		statuses []BackupStatus
		err      string
		want     JobRunStatus
	}{
		{"ok", []BackupStatus{StatusCompleted, StatusCompleted}, "", JobRunOK},
		{"partial", []BackupStatus{StatusCompleted, StatusFailed}, "", JobRunPartial},
		{"failed", []BackupStatus{StatusFailed, StatusFailed}, "", JobRunFailed},
		{"cancelled", []BackupStatus{StatusCompleted, StatusCancelled}, "", JobRunCancelled},
		{"failed and cancelled", []BackupStatus{StatusFailed, StatusCancelled}, "", JobRunFailed},
		{"nothing", nil, "", JobRunFailed},
		{"run error", nil, "cannot list databases", JobRunFailed},
	} {
		run := &JobRun{StartedAt: at, Error: tt.err}
		for i, st := range tt.statuses {
			run.Databases = append(run.Databases, JobRunDatabase{Database: string(rune('a' + i)), Status: st})
		}
		run.Finish(at.Add(time.Minute))
		if run.Status != tt.want || run.CompletedAt == nil || run.DurationSeconds != 60 {
			t.Errorf("%s: %+v; want %s", tt.name, run, tt.want)
		}
	}
	id, err := NewRunID(at)
	if err != nil || !strings.HasPrefix(id, "run_20260930_120000_") || ValidateID(id) != nil {
		t.Errorf("NewRunID = %q, %v", id, err)
	}
}
