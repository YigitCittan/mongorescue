package mongotools

import (
	"errors"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/mongouri"
)

// fuzzURIs are connection strings from the table tests plus known-tricky shapes.
var fuzzURIs = []string{
	"mongodb://localhost:27017",
	"mongodb://u:p%40ss@h1:1,h2:2/db",
	"mongodb://h/db?authSource=admin",
	"mongodb://h/?",
	"mongodb+srv://cluster.example.net/?retryWrites=true",
	"mongodb://h/?connecttimeoutms=1&SERVERSELECTIONTIMEOUTMS=2",
	"mongodb://sa:pw@h/my%20db",
	"mongodb://sa:pw@h/my+db",
	"mongodb://sa:pw@h/?auth%53ource=other",
	"mongodb://CN=client@h/?authMechanism=MONGODB-X509",
	"mongodb://sa:pw@h/?authMechanism=SCRAM%2DSHA%2D256",
	"mongodb://sa:pw@h?tls=true",
	"mongodb://sa:pw@h/mydb?",
	"mongodb://sa:pw@h/?authSource=admin&serverSelectionTimeoutMS=1&connectTimeoutMS=2",
	"mongodb://u:it's-secret@h:27017/db?authSource=admin",
	"mongodb://u:p''@h/\u00e9",
	"mongodb://h/\nuri: evil",
	"mongodb://u:p\u2028x@h",
	"mongodb://u:p\u0085x@h",
	"mongodb://u:p\xff@h",
	"mongodb://u:p\ufeff@h",
}

// FuzzWithConnectionDefaults checks that WithConnectionDefaults never panics, only
// appends options (so the credentials are unchanged), keeps a valid URI valid, sets
// each default option exactly once and is idempotent.
func FuzzWithConnectionDefaults(f *testing.F) {
	for _, s := range fuzzURIs {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		out := WithConnectionDefaults(uri)
		if !strings.HasPrefix(out, uri) {
			t.Fatalf("WithConnectionDefaults(%q) = %q changed the input", uri, out)
		}
		if mongouri.Validate(uri) != nil {
			return
		}
		if err := mongouri.Validate(out); err != nil {
			t.Fatalf("WithConnectionDefaults(%q) = %q is no longer valid: %v", uri, out, err)
		}
		if again := WithConnectionDefaults(out); again != out {
			t.Fatalf("WithConnectionDefaults is not idempotent: %q then %q", out, again)
		}
		counts := optionCounts(t, out)
		for _, key := range []string{"serverselectiontimeoutms", "connecttimeoutms"} {
			if counts[key] < 1 {
				t.Fatalf("WithConnectionDefaults(%q) = %q lacks %s", uri, out, key)
			}
		}
		// Options already present are never added again; missing ones at most once.
		in := optionCounts(t, uri)
		for key, n := range counts {
			if (in[key] > 0 && n != in[key]) || (in[key] == 0 && n > 1) {
				t.Fatalf("WithConnectionDefaults(%q) = %q sets %s %d times", uri, out, key, n)
			}
		}
	})
}

// optionCounts counts the query options of uri by lower-cased, unescaped name.
func optionCounts(t *testing.T, uri string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	_, rest, _ := strings.Cut(uri, "://")
	_, query, ok := strings.Cut(rest, "?")
	if !ok {
		return counts
	}
	for _, opt := range strings.Split(query, "&") {
		key, _, ok := strings.Cut(opt, "=")
		if !ok {
			continue
		}
		unescaped, err := url.QueryUnescape(key)
		if err != nil {
			t.Fatalf("undecodable option %q in %q: %v", key, uri, err)
		}
		counts[strings.ToLower(unescaped)]++
	}
	return counts
}

// FuzzWriteURIConfig checks that WriteURIConfig rejects exactly the URIs that cannot be
// written verbatim and that the file is a YAML single-quoted scalar holding uri.
func FuzzWriteURIConfig(f *testing.F) {
	for _, s := range fuzzURIs {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uri string) {
		dir := t.TempDir()
		arg, cleanup, err := WriteURIConfig(dir, uri)
		unsafe := !utf8.ValidString(uri) || strings.ContainsFunc(uri, unsafeInYAML)
		if err != nil {
			if !errors.Is(err, ErrInvalidURI) || !unsafe {
				t.Fatalf("WriteURIConfig(%q) = %v", uri, err)
			}
			return
		}
		defer cleanup()
		if unsafe {
			t.Fatalf("WriteURIConfig(%q) accepted a control character", uri)
		}
		path, ok := strings.CutPrefix(arg, "--config=")
		if !ok || !strings.HasPrefix(path, dir) {
			t.Fatalf("argument %q is not a config file in %s", arg, dir)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("config file mode = %v", info.Mode().Perm())
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: a file this test created.
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseSingleQuoted(string(data))
		if err != nil {
			t.Fatalf("config %q: %v", data, err)
		}
		if got != uri {
			t.Fatalf("config holds %q; want %q", got, uri)
		}
	})
}

// parseSingleQuoted parses the "uri: '<scalar>'\n" document WriteURIConfig writes,
// following the YAML rules for a single-line single-quoted scalar: a doubled quote is one quote,
// a lone quote ends the scalar and no line break may appear inside it.
func parseSingleQuoted(doc string) (string, error) {
	body, ok := strings.CutPrefix(doc, "uri: '")
	if !ok {
		return "", errors.New("missing key or opening quote")
	}
	body, ok = strings.CutSuffix(body, "'\n")
	if !ok {
		return "", errors.New("missing closing quote")
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '\'':
			if i+1 >= len(body) || body[i+1] != '\'' {
				return "", errors.New("unescaped quote inside the scalar")
			}
			b.WriteByte('\'')
			i++
		case '\n', '\r':
			return "", errors.New("line break inside the scalar")
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}
