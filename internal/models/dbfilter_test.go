package models

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDatabaseFilterJSONTakesNamesAndObjects(t *testing.T) {
	var got []DatabaseFilter
	in := `["a", {"name":"b","exclude_collections":["logs","tmp_1"]}, {"name":"c","collections":["orders"]}, {"name":"d"}]`
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	want := []DatabaseFilter{
		{Name: "a"},
		{Name: "b", ExcludeCollections: []string{"logs", "tmp_1"}},
		{Name: "c", Collections: []string{"orders"}},
		{Name: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v; want %+v", got, want)
	}
	// An entry without a filter is written as its name.
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `["a",{"name":"b","exclude_collections":["logs","tmp_1"]},{"name":"c","collections":["orders"]},"d"]` {
		t.Errorf("encoded %s", out)
	}
	for _, bad := range []string{`[1]`, `[["a"]]`, `[true]`, `[{"name":1}]`} {
		var v []DatabaseFilter
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("%s decoded to %+v; want an error", bad, v)
		}
	}
	var null []DatabaseFilter
	if err := json.Unmarshal([]byte(`[null]`), &null); err == nil {
		t.Errorf("[null] decoded to %+v; want an error", null)
	}
}

func TestDatabaseSelectionJSONIsBackwardCompatible(t *testing.T) {
	// A selection stored before collection filters (names only) decodes unchanged.
	var old DatabaseSelection
	if err := json.Unmarshal([]byte(`{"mode":"list","databases":["a","b"],"auto_include_new":false}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Mode != SelectionList || !reflect.DeepEqual(old.Databases, []string{"a", "b"}) || old.CollectionFilters != nil {
		t.Fatalf("old selection = %+v", old)
	}
	out, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"mode":"list","databases":["a","b"],"auto_include_new":false}` {
		t.Errorf("old selection encoded as %s", out)
	}

	// Object entries move their filter to collection_filters.
	var sel DatabaseSelection
	in := `{"mode":"list","databases":["a",{"name":"b","exclude_collections":["logs"]}]}`
	if err = json.Unmarshal([]byte(in), &sel); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sel.Databases, []string{"a", "b"}) ||
		!reflect.DeepEqual(sel.CollectionFilters, []DatabaseFilter{{Name: "b", ExcludeCollections: []string{"logs"}}}) {
		t.Fatalf("selection = %+v", sel)
	}
	// The stored form round-trips.
	out, err = json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	var back DatabaseSelection
	if err = json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, sel) {
		t.Errorf("round trip of %s = %+v; want %+v", out, back, sel)
	}
	if f, ok := sel.FilterFor("b"); !ok || f.ExcludeCollections[0] != "logs" {
		t.Errorf("FilterFor(b) = %+v, %v", f, ok)
	}
	if _, ok := sel.FilterFor("a"); ok {
		t.Error("FilterFor(a) found a filter")
	}
}

func TestNormalizeChecksCollectionFilters(t *testing.T) {
	ok := []DatabaseSelection{
		{Mode: SelectionList, Databases: []string{"a", "b"}, CollectionFilters: []DatabaseFilter{{Name: "b", Collections: []string{" orders ", "orders"}}}},
		{Mode: SelectionAll, Exclude: []string{"tmp_*"}, CollectionFilters: []DatabaseFilter{{Name: "shop", ExcludeCollections: []string{"logs"}}}},
		// A database an all selection names is backed up even when excluded.
		{Mode: SelectionAll, Exclude: []string{"shop"}, Databases: []string{"shop"}, CollectionFilters: []DatabaseFilter{{Name: "shop", ExcludeCollections: []string{"logs"}}}},
		// A filter that filters nothing is dropped.
		{Mode: SelectionList, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "a"}}},
	}
	for i, sel := range ok {
		if err := sel.Normalize(); err != nil {
			t.Errorf("selection %d: %v", i, err)
		}
	}
	first := ok[0]
	_ = first.Normalize()
	if !reflect.DeepEqual(first.CollectionFilters, []DatabaseFilter{{Name: "b", Collections: []string{"orders"}}}) {
		t.Errorf("normalized filters = %+v", first.CollectionFilters)
	}
	dropped := ok[3]
	_ = dropped.Normalize()
	if dropped.CollectionFilters != nil {
		t.Errorf("empty filter kept: %+v", dropped.CollectionFilters)
	}

	bad := map[string]DatabaseSelection{
		"not listed":     {Mode: SelectionList, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "b", Collections: []string{"x"}}}},
		"twice":          {Mode: SelectionList, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "a", Collections: []string{"x"}}, {Name: "a", ExcludeCollections: []string{"y"}}}},
		"pattern":        {Mode: SelectionPattern, Include: []string{"p_*"}, CollectionFilters: []DatabaseFilter{{Name: "p_a", Collections: []string{"x"}}}},
		"single":         {Mode: SelectionSingle, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "a", Collections: []string{"x"}}}},
		"system":         {Mode: SelectionAll, CollectionFilters: []DatabaseFilter{{Name: "admin", Collections: []string{"x"}}}},
		"excluded":       {Mode: SelectionAll, Exclude: []string{"tmp_*"}, CollectionFilters: []DatabaseFilter{{Name: "tmp_a", Collections: []string{"x"}}}},
		"bad collection": {Mode: SelectionList, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "a", ExcludeCollections: []string{"$x"}}}},
		"bad database":   {Mode: SelectionAll, CollectionFilters: []DatabaseFilter{{Name: "a.b", Collections: []string{"x"}}}},
	}
	for name, sel := range bad {
		if err := sel.Normalize(); !errors.Is(err, ErrInvalidSelection) {
			t.Errorf("%s: %v; want ErrInvalidSelection", name, err)
		}
	}
}

func TestSelectionCloneCopiesFilters(t *testing.T) {
	sel := DatabaseSelection{Mode: SelectionList, Databases: []string{"a"}, CollectionFilters: []DatabaseFilter{{Name: "a", Collections: []string{"x"}}}}
	c := sel.Clone()
	c.CollectionFilters[0].Collections[0] = "changed"
	if sel.CollectionFilters[0].Collections[0] != "x" {
		t.Error("Clone shares the filters")
	}
}
