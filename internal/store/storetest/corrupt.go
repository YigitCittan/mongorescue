package storetest

import (
	"context"
	"database/sql"
	"net/url"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/store"
)

// CorruptRow overwrites the data column of row id in table with data, bypassing the
// store (as a damaged or hand-edited database would). data must still be valid JSON
// (the tables CHECK json_valid); use a value of the wrong type, for example
// {"id":"job_1","name":42}, to make the row undecodable. table is a test constant.
func CorruptRow(tb testing.TB, s *store.SQLiteStore, table, id, data string) {
	tb.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", s.Path()+"?"+q.Encode())
	if err != nil {
		tb.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(), "UPDATE "+table+" SET data = ? WHERE id = ?", data, id) //nolint:gosec // G202: test-only table constant.
	if err != nil {
		tb.Fatalf("corrupt %s/%s: %v", table, id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		tb.Fatalf("corrupt %s/%s: no such row", table, id)
	}
}
