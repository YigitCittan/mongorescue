package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
)

func TestListAllJobsFollowsTheNextCursor(t *testing.T) {
	var queries []url.Values
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/v1/jobs" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		q := r.URL.Query()
		queries = append(queries, q)
		switch q.Get("after") {
		case "":
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"a","name":"a","extra":1},{"id":"b","name":"b"}],"meta":{"total":3,"limit":500,"next":"c1"}}`)
		case "c1":
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"c","name":"c"}],"meta":{"total":3,"limit":500}}`)
		default:
			t.Errorf("unexpected cursor %q", q.Get("after"))
		}
	})
	res, err := c.ListAllJobs(context.Background(), url.Values{"q": {"x"}, "limit": {"3"}, "after": {"stale"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Value) != 3 || res.Value[2].ID != "c" || res.Meta.Total != 3 {
		t.Fatalf("all jobs = %+v, meta %+v", res.Value, res.Meta)
	}
	var raw []map[string]any
	if err := json.Unmarshal(res.Raw, &raw); err != nil || len(raw) != 3 || raw[0]["extra"] != float64(1) {
		t.Fatalf("raw = %s, %v", res.Raw, err)
	}
	if len(queries) != 2 || queries[0].Get("limit") != "500" || queries[0].Get("after") != "" || queries[0].Get("q") != "x" || queries[1].Get("q") != "x" {
		t.Fatalf("queries = %v", queries)
	}
}

func TestListAllJobsStopsOnARepeatedCursor(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":[],"meta":{"total":1,"limit":500,"next":"same"}}`)
	})
	if _, err := c.ListAllJobs(context.Background(), nil); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("err = %v; want ErrUnexpectedResponse", err)
	}
}

func TestListAllJobsEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":[],"meta":{"total":0,"limit":500}}`)
	})
	res, err := c.ListAllJobs(context.Background(), nil)
	if err != nil || len(res.Value) != 0 || string(res.Raw) != "[]" {
		t.Fatalf("empty = %+v, %v", res, err)
	}
}
