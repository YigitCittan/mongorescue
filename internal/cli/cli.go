// Package cli implements the "mongorescue backup|restore|list|verify|status"
// commands: a client of a running MongoRescue instance that talks to its REST API
// with an API key (see docs/cli.md). It never opens the data directory and never
// reads MONGORESCUE_API_KEY or MONGORESCUE_API_KEY_FILE, which the server imports as
// an admin key. Exit codes are a stable contract (the Exit constants).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Exit codes of the CLI. They are a stable contract for scripts and CI.
const (
	// ExitOK means the command succeeded.
	ExitOK = 0
	// ExitFailed means the operation failed: a backup or restore failed, a
	// verification found a mismatch, a restore preflight failed, or the server
	// refused the request as invalid (400, 422, 429).
	ExitFailed = 1
	// ExitUsage means invalid arguments or flags, or a missing API key.
	ExitUsage = 2
	// ExitAuth means the server refused the API key (401) or its scope (403).
	ExitAuth = 3
	// ExitNotFound means the backup, restore or job does not exist (404).
	ExitNotFound = 4
	// ExitConflict means the request conflicts with what runs (409).
	ExitConflict = 5
	// ExitWaitStopped means --wait ended (--timeout or an interrupt) before the run
	// finished; the run goes on on the server.
	ExitWaitStopped = 6
	// ExitUnavailable means the server is unreachable, answered 5xx, or is not a
	// MongoRescue API at the URL.
	ExitUnavailable = 7
)

// Environment variables read by the CLI. MONGORESCUE_API_KEY and
// MONGORESCUE_API_KEY_FILE are never read: the server imports them as an admin key.
const (
	// EnvURL overrides the default --url.
	EnvURL = "MONGORESCUE_URL"
	// EnvAPIKeyFile names a file holding the API key.
	EnvAPIKeyFile = "MONGORESCUE_CLI_API_KEY_FILE" //nolint:gosec // G101: an environment variable name, not a credential.
	// EnvAPIKey holds the API key.
	EnvAPIKey = "MONGORESCUE_CLI_API_KEY" //nolint:gosec // G101: an environment variable name, not a credential.
)

// DefaultPollInterval is how often --wait polls the server.
const DefaultPollInterval = 2 * time.Second

// Errors of the CLI, mapped to exit codes by ExitCode.
var (
	// ErrUsage marks invalid arguments, flags or configuration (ExitUsage).
	ErrUsage = errors.New("usage")
	// ErrFailed marks an operation that ran and failed (ExitFailed).
	ErrFailed = errors.New("failed")
	// ErrWaitStopped marks a wait that ended before the run finished
	// (ExitWaitStopped).
	ErrWaitStopped = errors.New("stopped waiting")
	// errHelp is returned when -h or --help was asked for (ExitOK).
	errHelp = errors.New("help requested")
)

// commandNames lists the commands Run dispatches, in the order of the usage text.
var commandNames = []string{"backup", "restore", "list", "verify", "status", "help"}

// IsCommand reports whether name is a CLI command ("help" included).
func IsCommand(name string) bool {
	return slices.Contains(commandNames, name)
}

// App runs CLI commands. The zero value is ready to use.
type App struct {
	// Version is the CLI's build version, sent in the User-Agent.
	Version string
	// PollInterval is how often --wait polls; zero is DefaultPollInterval.
	PollInterval time.Duration
	// HTTPClient overrides the HTTP client (tests); nil uses the apiclient default.
	HTTPClient *http.Client
}

// Run runs the CLI command in args[0] with an App of no version; see App.Run.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return (&App{}).Run(ctx, args, getenv, stdout, stderr)
}

// command runs one command with its arguments (without the command name).
type command func(ctx context.Context, s *session, args []string) error

// Run runs the command named by args[0] with the rest of args, writing its result to
// stdout and diagnostics and progress to stderr, and returns the exit code. ctx
// bounds every request; cancelling it while --wait polls stops only the waiting.
func (a *App) Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return ExitUsage
	}
	commands := map[string]command{
		"backup": runBackup, "restore": runRestore, "list": runList, "verify": runVerify, "status": runStatus,
	}
	s := &session{app: a, stdout: stdout, stderr: stderr, getenv: getenv}
	name, rest := args[0], args[1:]
	if name == "help" {
		if len(rest) == 0 {
			writeUsage(stdout)
			return ExitOK
		}
		if _, ok := commands[rest[0]]; !ok {
			fmt.Fprintf(stderr, "mongorescue: unknown command %q\n\n", rest[0])
			writeUsage(stderr)
			return ExitUsage
		}
		name, rest = rest[0], []string{"-h"}
		s.stderr = stdout
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "mongorescue: unknown command %q\n\n", name)
		writeUsage(stderr)
		return ExitUsage
	}
	err := cmd(ctx, s, rest)
	if err == nil || errors.Is(err, errHelp) {
		return ExitOK
	}
	code := ExitCode(err)
	var done *reportedError
	if !errors.As(err, &done) {
		fmt.Fprintln(stderr, "mongorescue: "+redact.Text(err.Error()))
		if hint := hintFor(code, s); hint != "" {
			fmt.Fprintln(stderr, hint)
		}
	}
	return code
}

// reportedError is an error whose explanation the command already wrote; Run only
// maps it to its exit code.
type reportedError struct{ err error }

// Error implements error.
func (e *reportedError) Error() string { return e.err.Error() }

// Unwrap returns the error.
func (e *reportedError) Unwrap() error { return e.err }

// reportedInText returns err as already reported in text mode, where the summary on
// stdout says what failed; with --json or --quiet it is still written to stderr.
func (s *session) reportedInText(err error) error {
	if s.opts.json || s.opts.quiet {
		return err
	}
	return &reportedError{err: err}
}

// ExitCode maps an error of a command to its exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrUsage), errors.Is(err, apiclient.ErrInvalidConfig):
		return ExitUsage
	case errors.Is(err, ErrWaitStopped):
		return ExitWaitStopped
	case errors.Is(err, apiclient.ErrUnauthorized), errors.Is(err, apiclient.ErrForbidden):
		return ExitAuth
	case errors.Is(err, apiclient.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, apiclient.ErrConflict):
		return ExitConflict
	case errors.Is(err, apiclient.ErrUnreachable), errors.Is(err, apiclient.ErrServer),
		errors.Is(err, apiclient.ErrUnexpectedResponse):
		return ExitUnavailable
	}
	return ExitFailed
}

// hintFor returns a line of advice for an exit code, or "".
func hintFor(code int, s *session) string {
	switch code {
	case ExitUsage:
		return `Run "mongorescue help" for usage.`
	case ExitAuth:
		return "Check the API key (" + EnvAPIKeyFile + " or " + EnvAPIKey + ") and its scope (Settings → API keys)."
	case ExitUnavailable:
		if s.baseURL != "" {
			return "Is MongoRescue running at " + s.baseURL + "? Set --url or " + EnvURL + "."
		}
	}
	return ""
}

// usageErrorf returns an ErrUsage error with a message.
func usageErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, args...))
}

// failedErrorf returns an ErrFailed error with a message.
func failedErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFailed, fmt.Sprintf(format, args...))
}

// writeUsage writes the overview of the commands.
func writeUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: mongorescue <command> [arguments] [flags]

Commands for a running MongoRescue instance, through its REST API and an API key:
  backup    Start a backup of a job (--job) or of one database (--connection, --database)
  restore   Restore a backup into a safe clone (in place only with --in-place --confirm)
  list      List backups, restores, jobs, connections or targets
  verify    Re-read a backup's archive and compare it with its checksum
  status    Show the server's health, the API key's scope, counts and active runs
  help      Show the flags of a command: mongorescue help <command>

Every command takes:
  --url URL            Base URL of the instance (env %s, default %s)
  --api-key-file PATH  File holding the API key (env %s, or the key in %s)
  --json               Print the server's JSON instead of text
  --quiet              Print only IDs; no progress

backup, restore and verify also take --wait (poll until the run finishes, progress
on stderr) and --timeout DURATION (stop waiting after it, exit 6).

Exit codes: 0 ok, 1 operation failed, 2 usage, 3 unauthorized or forbidden,
4 not found, 5 conflict, 6 wait stopped, 7 unreachable or server error.

Without a command mongorescue runs the server ("mongorescue -h" lists its flags);
"mongorescue mcp" runs the MCP stdio bridge. See docs/cli.md.
`, EnvURL, apiclient.DefaultURL, EnvAPIKeyFile, EnvAPIKey)
}

// newFlagSet returns a flag set for a command whose usage is usage (the synopsis and
// a description) and the shared flags; withWait adds --wait and --timeout.
func (s *session) newFlagSet(name, usage string, withWait bool) *flag.FlagSet {
	fs := flag.NewFlagSet("mongorescue "+name, flag.ContinueOnError)
	fs.SetOutput(s.stderr)
	fs.Usage = func() {
		fmt.Fprintf(s.stderr, "%s\nFlags:\n", usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&s.opts.url, "url", "", "Base URL of the MongoRescue instance (env "+EnvURL+", default "+apiclient.DefaultURL+")")
	fs.StringVar(&s.opts.apiKeyFile, "api-key-file", "", "File holding the API key (env "+EnvAPIKeyFile+"; or set "+EnvAPIKey+")")
	fs.StringVar(&s.opts.apiKey, "api-key", "", "API key (discouraged: visible in the process list; used only when no other source is set)")
	fs.BoolVar(&s.opts.json, "json", false, "Print the server's JSON")
	fs.BoolVar(&s.opts.quiet, "quiet", false, "Print only IDs; no progress")
	if withWait {
		fs.BoolVar(&s.opts.wait, "wait", false, "Wait until the run finishes, with progress on stderr")
		fs.DurationVar(&s.opts.timeout, "timeout", 0, "With --wait: stop waiting after this long (exit 6); 0 waits until the run finishes")
	}
	return fs
}

// parse parses args with fs, allowing flags after positional arguments, and checks
// the shared flags. It returns the positional arguments.
func (s *session) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	s.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { s.set[f.Name] = true })
	if s.opts.json && s.opts.quiet {
		return nil, usageErrorf("--json and --quiet cannot be combined")
	}
	if s.set["timeout"] && !s.opts.wait {
		return nil, usageErrorf("--timeout needs --wait")
	}
	if s.opts.timeout < 0 {
		return nil, usageErrorf("--timeout must not be negative")
	}
	return pos, nil
}
