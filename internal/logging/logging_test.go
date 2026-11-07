package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secretURI = "mongodb://admin:hunter2secret@db.example.com:27017/app?authSource=admin"

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{"": FormatText, "text": FormatText, " JSON ": FormatJSON, "json": FormatJSON} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFormat("xml"); !errors.Is(err, ErrInvalidFormat) {
		t.Errorf("ParseFormat(xml) error = %v, want ErrInvalidFormat", err)
	}
}

type stringer struct{}

func (stringer) String() string { return "dsn " + secretURI }

func TestRedactionInBothFormats(t *testing.T) {
	for _, format := range []Format{FormatText, FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			l := New(&buf, format, slog.LevelDebug)
			l.Info("connecting to "+secretURI, slog.String("uri", secretURI), slog.Any("error", fmt.Errorf("dial: %w", errors.New(secretURI))),
				slog.Any("dsn", stringer{}), slog.Group("g", slog.String("nested", secretURI)))
			l.With("uri", secretURI).Warn("with")
			if strings.Contains(buf.String(), "hunter2secret") {
				t.Fatalf("password leaked in %s output: %s", format, buf.String())
			}
			if !strings.Contains(buf.String(), "******") {
				t.Fatalf("no mask in output: %s", buf.String())
			}
		})
	}
}

func TestJSONParsesLineByLineWithCorrelation(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatJSON, slog.LevelInfo)
	ctx := WithRun(WithRequestID(context.Background(), "req-1"), "run_1", "job_1")
	l.InfoContext(ctx, "one", slog.String("run_id", "explicit"))
	l.ErrorContext(context.Background(), "two\nwith newline")
	sc := bufio.NewScanner(&buf)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0]["request_id"] != "req-1" || lines[0]["job_id"] != "job_1" || lines[0]["run_id"] != "explicit" {
		t.Errorf("first line = %v", lines[0])
	}
	if _, ok := lines[1]["request_id"]; ok {
		t.Errorf("second line has a request_id: %v", lines[1])
	}
}

func TestContextAttrsInText(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatText, slog.LevelInfo).With("component", "x").WithGroup("g")
	l.InfoContext(WithRequestID(context.Background(), "abc"), "hello")
	if !strings.Contains(buf.String(), "request_id=abc") || !strings.Contains(buf.String(), "component=x") {
		t.Errorf("output = %s", buf.String())
	}
}
