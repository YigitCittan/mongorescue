package redact

import (
	"strings"
	"testing"
)

// fuzzURIs are inputs from the table tests plus known-tricky shapes.
var fuzzURIs = []string{
	"",
	"localhost:27017",
	"mongodb://localhost:27017/db",
	"mongodb://app@h/db",
	"mongodb://u:secret@h/db",
	"mongodb+srv://u:p%40ss@c.net/?w=1",
	"mongodb://h/db?authSource=admin&password=hunter2&w=1",
	"mongodb://h/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:tok123,SERVICE_NAME:x&w=1",
	"mongodb://u:pa/ss@h/db",
	"mongodb://u:pa?ss@h/db",
	"mongodb://u:pa#ss@h/db",
	"mongodb://u:p@ss@h1,h2/db",
	"mongodb://h/?pass%77ord=x&w=1",
	"mongodb://h/?password%zz=x&secretKey=y",
	"mongodb://h/?password=x#frag",
	"failed mongodb://a:pass1@h1:27017/db then retried mongodb+srv://b:pass2@c.net/x",
	"mongodb://a:p1@h1,mongodb://b:p2@h2",
	"mongodb://u:ab://c@h",
	"mongodb://a?password=b:c@h",
	"x mongodb://h/?PASSWORD=s1 y",
	"open /tmp/x?password=1",
}

// FuzzURI checks that URI and Text never panic and are idempotent.
func FuzzURI(f *testing.F) {
	for _, s := range fuzzURIs {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := URI(s)
		if twice := URI(once); twice != once {
			t.Fatalf("URI is not idempotent for %q: %q then %q", s, once, twice)
		}
		onceText := Text(s)
		if twice := Text(onceText); twice != onceText {
			t.Fatalf("Text is not idempotent for %q: %q then %q", s, onceText, twice)
		}
	})
}

// FuzzURIPassword checks that URI never returns the password of
// "mongodb://<user>:<password>@<rest>", whatever characters the parts contain.
func FuzzURIPassword(f *testing.F) {
	f.Add("u", "secret", "h/db")
	f.Add("u", "pa/ss", "h/db?w=1")
	f.Add("u", "p@ss", "h1,h2/db")
	f.Add("", "pa?ss#x", "h")
	f.Add("u:x", "y", "h/?password=z")
	f.Add("u", "p", "h/?appName=me@corp")
	f.Add("a?password=b", "c", "h")
	f.Fuzz(func(t *testing.T, user, password, rest string) {
		if password == "" || strings.Contains(password, "*") {
			return
		}
		in := "mongodb://" + user + ":" + password + "@" + rest
		out := URI(in)
		shell := "mongodb://" + user + ":" + Mask + "@" + rest
		if strings.Contains(out, password) && !strings.Contains(shell, password) {
			t.Fatalf("URI(%q) = %q leaks the password %q", in, out, password)
		}
	})
}

// FuzzTextPassword checks that Text never returns the password of a connection
// string embedded in free text. Passwords with whitespace are masked only up to the
// first space (a documented limitation), and a raw '@' in a password makes adjacent
// URIs ambiguous, so both are skipped.
func FuzzTextPassword(f *testing.F) {
	f.Add("error: ", "u", "secret", "h/db", " failed")
	f.Add("", "u", "ab://c", "h", "")
	f.Add("a://b:", "u", "p", "h", "")
	f.Add("a://b@", "u", "p", "h", ",mongodb://c:d@e")
	f.Add("x", "q://w", "p", "h/x://z:q@w", "")
	f.Add("dial ", "", "pa/ss?x", "h/?password=q", "\n")
	f.Fuzz(func(t *testing.T, prefix, user, password, rest, suffix string) {
		if password == "" || strings.ContainsAny(password, "*@") || strings.ContainsFunc(password, isSpace) ||
			strings.ContainsAny(user, "@") || strings.ContainsFunc(user, isSpace) {
			return
		}
		in := prefix + "mongodb://" + user + ":" + password + "@" + rest + suffix
		out := Text(in)
		shell := prefix + "mongodb://" + user + ":" + Mask + "@" + rest + suffix
		if strings.Contains(out, password) && !strings.Contains(shell, password) {
			t.Fatalf("Text(%q) = %q leaks the password %q", in, out, password)
		}
	})
}
