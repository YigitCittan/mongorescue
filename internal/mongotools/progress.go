package mongotools

import (
	"regexp"
	"strconv"
	"strings"
)

// ProgressKind classifies a progress line of mongodump or mongorestore.
type ProgressKind int

// Progress line kinds.
const (
	// ProgressStarted is a collection the tool started ("writing db.c to archive",
	// "restoring db.c from archive").
	ProgressStarted ProgressKind = iota + 1
	// ProgressBar is a progress bar line: "[####....]  db.c  1234/5678  (21.7%)".
	// mongodump counts documents, mongorestore bytes ("1.2MB/4.5MB").
	ProgressBar
	// ProgressDone is a finished collection ("done dumping db.c (N documents)",
	// "finished restoring db.c (N documents, M failures)").
	ProgressDone
)

// Progress is one parsed progress line.
type Progress struct {
	// Kind classifies the line.
	Kind ProgressKind
	// Namespace is the collection the line is about ("db.collection").
	Namespace string
	// Done and Total are the bar's counters: documents for mongodump, bytes for
	// mongorestore (see Bytes).
	Done, Total float64
	// Bytes reports that Done and Total are byte amounts.
	Bytes bool
	// Percent is the bar's percentage.
	Percent float64
	// Documents is the document count of a done line.
	Documents int64
	// Failures is the failed document count of a mongorestore done line.
	Failures int64
}

var (
	// barPattern matches "[####....]  db.c  1234/5678  (21.7%)" and the byte form
	// "[##..]  db.c  1.20MB/4.50MB  (26.7%)".
	barPattern = regexp.MustCompile(`^\[[#.]*\]\s+(\S+)\s+([0-9.]+)([KMGTP]?i?B)?/([0-9.]+)([KMGTP]?i?B)?\s+\(\s*([0-9.]+)%\)\s*$`)
	// writingPattern matches mongodump's "writing db.c to archive on stdout" (and to a
	// directory, "writing db.c to dump/db/c.bson").
	writingPattern = regexp.MustCompile(`^writing (\S+) to `)
	// doneDumpingPattern matches "done dumping db.c (123 documents)".
	doneDumpingPattern = regexp.MustCompile(`^done dumping (\S+) \((\d+) documents?\)`)
	// restoringPattern matches "restoring db.c from archive on stdin".
	restoringPattern = regexp.MustCompile(`^restoring (\S+) from `)
	// finishedRestoringPattern matches "finished restoring db.c (12 documents, 0 failures)".
	finishedRestoringPattern = regexp.MustCompile(`^finished restoring (\S+) \((\d+) documents?, (\d+) failures?\)`)
)

// ParseProgress parses one line of mongodump or mongorestore output (with or without
// the tools' leading timestamp). ok is false for lines that carry no progress.
func ParseProgress(line string) (p Progress, ok bool) {
	msg := strings.TrimSpace(stripTimestamp(line))
	switch {
	case strings.HasPrefix(msg, "["):
		m := barPattern.FindStringSubmatch(msg)
		if m == nil {
			return p, false
		}
		done, errDone := strconv.ParseFloat(m[2], 64)
		total, errTotal := strconv.ParseFloat(m[4], 64)
		pct, errPct := strconv.ParseFloat(m[6], 64)
		if errDone != nil || errTotal != nil || errPct != nil {
			return p, false
		}
		p = Progress{Kind: ProgressBar, Namespace: m[1], Done: done, Total: total, Percent: pct}
		if m[3] != "" || m[5] != "" {
			p.Bytes = true
			p.Done *= byteUnit(m[3])
			p.Total *= byteUnit(m[5])
		}
		return p, true
	case strings.HasPrefix(msg, "writing "):
		if m := writingPattern.FindStringSubmatch(msg); m != nil {
			return Progress{Kind: ProgressStarted, Namespace: m[1]}, true
		}
	case strings.HasPrefix(msg, "done dumping "):
		if m := doneDumpingPattern.FindStringSubmatch(msg); m != nil {
			docs, _ := strconv.ParseInt(m[2], 10, 64)
			return Progress{Kind: ProgressDone, Namespace: m[1], Documents: docs}, true
		}
	case strings.HasPrefix(msg, "restoring "):
		// "restoring indexes for collection db.c from metadata" and "restoring users
		// from archive" name no namespace.
		if m := restoringPattern.FindStringSubmatch(msg); m != nil && strings.Contains(m[1], ".") {
			return Progress{Kind: ProgressStarted, Namespace: m[1]}, true
		}
	case strings.HasPrefix(msg, "finished restoring "):
		if m := finishedRestoringPattern.FindStringSubmatch(msg); m != nil {
			docs, _ := strconv.ParseInt(m[2], 10, 64)
			failures, _ := strconv.ParseInt(m[3], 10, 64)
			return Progress{Kind: ProgressDone, Namespace: m[1], Documents: docs, Failures: failures}, true
		}
	}
	return p, false
}

// stripTimestamp removes the tools' "2006-01-02T15:04:05.000-0700<TAB>" prefix.
func stripTimestamp(line string) string {
	if before, after, found := strings.Cut(line, "\t"); found && len(before) >= 10 && before[0] >= '0' && before[0] <= '9' && strings.Contains(before, "T") {
		return after
	}
	return line
}

// byteUnit returns the multiplier of a size unit printed by the tools (B, KB, MB, ...
// in powers of 1024, as the tools compute them).
func byteUnit(unit string) float64 {
	switch strings.TrimSuffix(strings.TrimSuffix(unit, "B"), "i") {
	case "K":
		return 1 << 10
	case "M":
		return 1 << 20
	case "G":
		return 1 << 30
	case "T":
		return 1 << 40
	case "P":
		return 1 << 50
	default:
		return 1
	}
}
