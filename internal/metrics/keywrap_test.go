package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestKeyWrapSeries(t *testing.T) {
	m := New(BuildInfo{Version: "1", Commit: "c", GoVersion: "go"})
	if strings.Contains(scrape(t, m), "mongorescue_key_wrapping_active{") {
		t.Fatal("key_wrapping_active is reported without key wrapping")
	}
	m.SetKeyWrapping("aws-kms")
	m.ObserveKeyWrap("unwrap", "aws-kms", 120*time.Millisecond, nil)
	m.ObserveKeyWrap("unwrap", "aws-kms", time.Second, errors.New("down"))
	m.ObserveKeyWrap("wrap", "aws-kms", time.Millisecond, nil)
	body := scrape(t, m)
	for _, want := range []string{
		`mongorescue_key_wrapping_active{provider="aws-kms"} 1`,
		`mongorescue_key_wrap_errors_total{operation="unwrap",provider="aws-kms"} 1`,
		`mongorescue_key_wrap_errors_total{operation="wrap",provider="aws-kms"} 0`,
		`mongorescue_key_wrap_duration_seconds_count{operation="unwrap",provider="aws-kms"} 2`,
		`mongorescue_key_wrap_duration_seconds_count{operation="wrap",provider="aws-kms"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
}
