package models

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDatabaseFilterRefusesIncludeAndExclude(t *testing.T) {
	f := DatabaseFilter{Name: "shop", Collections: []string{"orders"}, ExcludeCollections: []string{"logs"}}
	if err := f.Normalize(); !errors.Is(err, ErrIncludeAndExclude) {
		t.Fatalf("Normalize = %v; want ErrIncludeAndExclude", err)
	}
	sel := DatabaseSelection{Mode: SelectionList, Databases: []string{"shop"}, CollectionFilters: []DatabaseFilter{f}}
	if err := sel.Normalize(); !errors.Is(err, ErrInvalidSelection) || !errors.Is(err, ErrIncludeAndExclude) {
		t.Fatalf("selection Normalize = %v; want ErrInvalidSelection and ErrIncludeAndExclude", err)
	}
}

func TestAllSelectionObjectEntriesOnlyAttachFilters(t *testing.T) {
	var sel DatabaseSelection
	in := `{"mode":"all","exclude":["tmp_*"],"databases":["pinned",{"name":"shop","exclude_collections":["logs"]},{"name":"plain"}]}`
	if err := json.Unmarshal([]byte(in), &sel); err != nil {
		t.Fatal(err)
	}
	// Names (and objects without a filter) are always backed up; a filter is not.
	if !reflect.DeepEqual(sel.Databases, []string{"pinned", "plain"}) {
		t.Errorf("databases = %q; a filter must not add shop to the always-backed-up list", sel.Databases)
	}
	if !reflect.DeepEqual(sel.CollectionFilters, []DatabaseFilter{{Name: "shop", ExcludeCollections: []string{"logs"}}}) {
		t.Errorf("filters = %+v", sel.CollectionFilters)
	}
	if err := sel.Normalize(); err != nil {
		t.Fatal(err)
	}
	// The filter does not change what the selection matches.
	plain := DatabaseSelection{Mode: SelectionAll, Exclude: []string{"tmp_*"}, Databases: []string{"pinned", "plain"}}
	if !sel.SameMatch(plain) {
		t.Error("a filter changed SameMatch")
	}

	var excluded DatabaseSelection
	in = `{"mode":"all","exclude":["tmp_*"],"databases":[{"name":"tmp_a","collections":["x"]}]}`
	if err := json.Unmarshal([]byte(in), &excluded); err != nil {
		t.Fatal(err)
	}
	if err := excluded.Normalize(); !errors.Is(err, ErrInvalidSelection) || !strings.Contains(err.Error(), "filter for an excluded database") {
		t.Errorf("filter for an excluded database = %v", err)
	}

	// A list selection still backs up the database of an object entry.
	var list DatabaseSelection
	if err := json.Unmarshal([]byte(`{"mode":"list","databases":[{"name":"shop","collections":["orders"]}]}`), &list); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Databases, []string{"shop"}) || len(list.CollectionFilters) != 1 {
		t.Errorf("list selection = %+v", list)
	}
}
