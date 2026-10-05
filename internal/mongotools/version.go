package mongotools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MinPITRToolsVersion is the oldest MongoDB Database Tools release point-in-time
// restores are tested with (docs/design/pitr.md).
const MinPITRToolsVersion = "100.12.0"

// ErrToolVersion is returned when a tool's version cannot be read from its output.
var ErrToolVersion = errors.New("mongotools: cannot read the tool version")

// toolVersionPattern matches the first line of "<tool> --version":
// "mongorestore version: 100.12.2".
var toolVersionPattern = regexp.MustCompile(`version:\s*v?(\d+\.\d+(?:\.\d+)?)`)

// ParseToolVersion returns the version in the output of "<tool> --version".
func ParseToolVersion(out string) (string, bool) {
	m := toolVersionPattern.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// VersionAtLeast reports whether the dotted version v is min or newer.
func VersionAtLeast(v, minimum string) bool {
	return compareVersions(v, minimum) >= 0
}

// maxVersionOutput bounds the output of "<tool> --version" that is kept.
const maxVersionOutput = 4096

// ToolVersion runs "<name> --version" (resolved like Resolve) and returns its
// version, such as "100.12.2". It fails with an error wrapping ErrToolNotFound or
// ErrToolVersion.
func (r *Resolver) ToolVersion(ctx context.Context, name string) (string, error) {
	path, err := r.Resolve(name)
	if err != nil {
		return "", err
	}
	cmd := Command(ctx, path, "--version")
	out := &limitedBuffer{max: maxVersionOutput}
	cmd.Stdout, cmd.Stderr = out, out
	if err := Start(cmd); err != nil {
		return "", fmt.Errorf("run %s --version: %w", name, err)
	}
	if err := Wait(ctx, cmd); err != nil {
		return "", fmt.Errorf("run %s --version: %w", name, err)
	}
	v, ok := ParseToolVersion(out.String())
	if !ok {
		return "", fmt.Errorf("%w: %s printed %q", ErrToolVersion, name, strings.TrimSpace(firstLine(out.String())))
	}
	return v, nil
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	b   []byte
	max int
}

// Write implements io.Writer; it never fails.
func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// String returns what was kept.
func (l *limitedBuffer) String() string { return string(l.b) }
