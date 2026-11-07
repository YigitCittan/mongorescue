package cli

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// helpFlags returns the flags "mongorescue help <command>" lists, parsed from the
// flag package's own usage output: a source independent of the completion table.
func helpFlags(t *testing.T, command string) []string {
	t.Helper()
	code, stdout, stderr := runCLI(context.Background(), t, http.NotFoundHandler(), nil, "help", command)
	if code != ExitOK {
		t.Fatalf("help %s: exit %d: %s", command, code, stderr)
	}
	re := regexp.MustCompile(`(?m)^  -([a-z][a-z0-9-]*)`)
	var flags []string
	for _, m := range re.FindAllStringSubmatch(stdout, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

// completionScript runs "mongorescue completion shell" and returns the script.
func completionScript(t *testing.T, app *App, shell string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"completion", shell}, func(string) string { return "" }, &stdout, &stderr); code != ExitOK {
		t.Fatalf("completion %s: exit %d: %s", shell, code, stderr.String())
	}
	return stdout.String()
}

func TestCompletionCoversEveryCommandAndFlag(t *testing.T) {
	app := &App{ExtraCommands: []ExtraCommand{{Name: "mcp", Summary: "MCP bridge"}}}
	table := app.commandTable()
	byName := map[string]commandInfo{}
	for _, c := range table {
		byName[c.name] = c
	}
	for _, name := range append(commandNamesOf(table), "completion") {
		if name != "mcp" && !IsCommand(name) {
			t.Errorf("%s is in the table but not a command", name)
		}
	}
	for _, d := range commandDocs {
		if _, ok := byName[d.name]; !ok {
			t.Errorf("command %s is missing from the completion table", d.name)
		}
	}
	for _, sh := range completionShells {
		script := completionScript(t, app, sh.name)
		if script == "" || !strings.HasSuffix(script, "\n") {
			t.Fatalf("%s: empty or unterminated script", sh.name)
		}
		for _, d := range commandDocs {
			if !strings.Contains(script, d.name) {
				t.Errorf("%s: command %s missing from the script", sh.name, d.name)
			}
		}
		if !strings.Contains(script, "mcp") {
			t.Errorf("%s: the extra command mcp is missing", sh.name)
		}
		for _, d := range commandDocs {
			if d.name == "help" || d.name == "completion" {
				continue
			}
			got := map[string]bool{}
			for _, f := range byName[d.name].flags {
				got[f.name] = true
			}
			want := helpFlags(t, d.name)
			if len(want) < 6 {
				t.Fatalf("help %s lists %d flags: the parse of its usage broke", d.name, len(want))
			}
			for _, f := range want {
				if !got[f] {
					t.Errorf("%s: flag --%s of %s is missing from the completion table", sh.name, f, d.name)
				}
				if !strings.Contains(script, "--"+f) && !strings.Contains(script, "-l "+f+" ") {
					t.Errorf("%s: flag --%s of %s is missing from the script", sh.name, f, d.name)
				}
			}
			for f := range got {
				if f != "help" && !slices.Contains(want, f) {
					t.Errorf("%s: the table has --%s for %s, which the command does not define", sh.name, f, d.name)
				}
			}
		}
	}
}

func TestCompletionScriptsAreStaticASCIIAndLF(t *testing.T) {
	app := &App{}
	for _, sh := range completionShells {
		script := completionScript(t, app, sh.name)
		if strings.ContainsRune(script, '\r') {
			t.Errorf("%s: the script has carriage returns", sh.name)
		}
		for i, r := range script {
			if r > 0x7e || (r < 0x20 && r != '\n') {
				t.Errorf("%s: byte %d is %q: scripts must be printable ASCII", sh.name, i, r)
				break
			}
		}
		if again := completionScript(t, app, sh.name); again != script {
			t.Errorf("%s: the script is not deterministic", sh.name)
		}
	}
}

func TestCompletionOffersListResourcesAndShells(t *testing.T) {
	app := &App{}
	for _, sh := range completionShells {
		script := completionScript(t, app, sh.name)
		for resource := range listResources {
			if !strings.Contains(script, resource) {
				t.Errorf("%s: list resource %s missing", sh.name, resource)
			}
		}
		for _, other := range completionShells {
			if !strings.Contains(script, other.name) {
				t.Errorf("%s: shell %s missing", sh.name, other.name)
			}
		}
	}
}

func TestCompletionUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"completion", "tcsh"}, {"completion", "bash", "zsh"}, {"completion", "--bogus"}} {
		if code, stdout, _ := runCLI(context.Background(), t, http.NotFoundHandler(), nil, args...); code != ExitUsage || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want %d and nothing", args, code, stdout, ExitUsage)
		}
	}
	if code, _, _ := runCLI(context.Background(), t, http.NotFoundHandler(), nil, "completion", "-h"); code != ExitOK {
		t.Errorf("completion -h: exit %d", code)
	}
	code, stdout, _ := runCLI(context.Background(), t, http.NotFoundHandler(), nil, "help", "completion")
	if code != ExitOK || !strings.Contains(stdout, "bash|zsh|fish|powershell") {
		t.Errorf("help completion: exit %d, %s", code, stdout)
	}
	code, stdout, _ = runCLI(context.Background(), t, http.NotFoundHandler(), nil, "help")
	if code != ExitOK || !strings.Contains(stdout, "completion") || !strings.Contains(stdout, "--no-color") {
		t.Errorf("help: exit %d, %s", code, stdout)
	}
}

func TestCompletionNeedsNoAPIKeyOrNetwork(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{HTTPClient: &http.Client{Transport: failTransport{t}}}
	code := app.Run(context.Background(), []string{"completion", "bash"}, func(string) string { return "" }, &stdout, &stderr)
	if code != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

// failTransport fails the test on any request.
type failTransport struct{ t *testing.T }

// RoundTrip implements http.RoundTripper.
func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
	return nil, context.Canceled
}

func TestShellQuoting(t *testing.T) {
	const text = `it's [a] "b" \ c`
	for name, got := range map[string]string{
		"zsh":  zshQuote(text),
		"fish": fishQuote(text),
		"ps":   psQuote(text),
	} {
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("%s: %s is not single-quoted", name, got)
		}
	}
	if got := zshQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("zshQuote = %s", got)
	}
	if got := fishQuote(`a'b\`); got != `'a\'b\\'` {
		t.Errorf("fishQuote = %s", got)
	}
	if got := psQuote("a'b"); got != "'a''b'" {
		t.Errorf("psQuote = %s", got)
	}
	if got := zshSpec("[x] y"); got != `[x\] y` {
		t.Errorf("zshSpec = %s", got)
	}
}

func TestShortDescription(t *testing.T) {
	if got := shortDescription("a\n  b"); got != "a b" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("word ", 40)
	got := shortDescription(long)
	if len(got) > 100 || !strings.HasSuffix(got, "...") {
		t.Errorf("got %q (%d)", got, len(got))
	}
}
