package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// awaitRecord polls a list endpoint (/api/v1/backups or /api/v1/restores) until the
// record with id leaves the in-progress state, and returns it as raw JSON fields.
// Operations started through the API run in the background and answer 202 at once.
func awaitRecord(t *testing.T, h http.Handler, listPath, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", listPath, nil))
		var res struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode %s: %v (%s)", listPath, err, rec.Body.String())
		}
		for _, r := range res.Data {
			if r["id"] == id && r["status"] != "in_progress" {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %s at %s did not finish in time (last: %s)", id, listPath, rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// acceptedID asserts a 202 Accepted response and returns the record ID it carries.
func acceptedID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var res struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Data.ID == "" {
		t.Fatalf("decode accepted record: %v (%s)", err, rec.Body.String())
	}
	if res.Data.Status != "in_progress" {
		t.Fatalf("accepted record status = %q, want in_progress", res.Data.Status)
	}
	return res.Data.ID
}
