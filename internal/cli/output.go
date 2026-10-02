package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// writeJSON writes raw JSON indented, followed by a newline; empty raw is null.
// Connection strings in its string values are redacted (the server already redacts
// what it sends; this is a second line of defence).
func writeJSON(w io.Writer, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, redactJSON(raw), "", "  "); err != nil {
		return fmt.Errorf("%w: the server's JSON cannot be printed: %w", ErrFailed, err)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// redactJSON returns raw with redact.Text applied to every string literal, keeping
// everything else (key order, numbers) byte for byte. Invalid JSON is returned as
// is, for json.Indent to refuse.
func redactJSON(raw []byte) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		end := i + 1
		for end < len(raw) && raw[end] != '"' {
			if raw[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(raw) {
			return raw
		}
		lit := raw[i : end+1]
		var s string
		if json.Unmarshal(lit, &s) == nil {
			if red := redact.Text(s); red != s {
				if enc, err := json.Marshal(red); err == nil {
					out = append(append(out, raw[last:i]...), enc...)
					last = end + 1
				}
			}
		}
		i = end
	}
	if out == nil {
		return raw
	}
	return append(out, raw[last:]...)
}

// table writes aligned columns.
type table struct {
	tw *tabwriter.Writer
}

// newTable starts a table with the header row.
func newTable(w io.Writer, header ...string) *table {
	t := &table{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
	t.row(header...)
	return t
}

// row adds a row; tabs and newlines in cells are replaced by spaces.
func (t *table) row(cells ...string) {
	for i, c := range cells {
		c = strings.Map(func(r rune) rune {
			if r == '\t' || r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, c)
		if c == "" {
			c = "-"
		}
		cells[i] = c
	}
	fmt.Fprintln(t.tw, strings.Join(cells, "\t"))
}

// flush writes the table.
func (t *table) flush() error {
	return t.tw.Flush()
}

// fmtTime formats t in UTC, or "-" for the zero time.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// fmtTimePtr formats *t, or "-" for nil.
func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return fmtTime(*t)
}

// fmtSeconds formats a duration in seconds, rounded to the second ("-" for none).
func fmtSeconds(sec float64) string {
	if sec <= 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return "-"
	}
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	if d < time.Second {
		return "<1s"
	}
	return d.String()
}

// fmtBytes formats a size in binary units.
func fmtBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v, unit := float64(n), 0
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for v /= 1024; v >= 1024 && unit < len(units)-1; v /= 1024 {
		unit++
	}
	return fmt.Sprintf("%.1f %s", v, units[unit])
}

// clean makes a server message printable: redacted, on one line.
func clean(s string) string {
	return strings.Join(strings.Fields(redact.Text(s)), " ")
}

// progressText describes a running backup or restore.
func progressText(status string, p *models.RunProgress) string {
	if p == nil {
		return status
	}
	parts := []string{p.Phase}
	if p.Percent != nil {
		parts = append(parts, fmt.Sprintf("%.0f%%", *p.Percent))
	}
	if p.Bytes > 0 {
		parts = append(parts, fmtBytes(p.Bytes))
	}
	if p.CurrentCollection != "" {
		parts = append(parts, p.CurrentCollection)
	}
	return status + " (" + strings.Join(parts, ", ") + ")"
}

// writeChecks writes the checks of a restore preflight.
func writeChecks(w io.Writer, p *models.PreflightResult) error {
	t := newTable(w, "CHECK", "STATUS", "MESSAGE")
	for _, c := range p.Checks {
		t.row(c.ID, string(c.Status), clean(c.Message))
	}
	return t.flush()
}
