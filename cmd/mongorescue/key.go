package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/provider"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// keyCommand is the name of the offline key wrapping commands.
const keyCommand = "key"

// openKeyWrapper opens the KMS wrapper of a spec (replaced in tests).
var openKeyWrapper keywrap.OpenFunc = provider.Open

// keyRetry bounds the KMS attempts of the commands (zero: keywrap.DefaultRetry;
// replaced in tests).
var keyRetry keywrap.Retry

// keyUsage describes "mongorescue key".
const keyUsage = `Usage: mongorescue key <command> [flags]

Offline commands for the KMS wrapping of secret.key (MONGORESCUE_KEY_WRAP). They work
on the data directory directly, so stop MongoRescue first: they refuse to run while
it holds the directory. They need the operating system permissions of the data
directory and an identity that may use the KMS keys involved.

Commands:
  status   Show how secret.key is stored (no KMS call).
  rewrap   Move secret.key.wrapped from the KMS key of ` + config.EnvKeyWrap + ` to another
           one: --to <provider>:<key URI>. Then set ` + config.EnvKeyWrap + ` to it.
  unwrap   Leave the KMS: write secret.key back in plain form and remove
           secret.key.wrapped. Needs --confirm-plaintext. Then unset ` + config.EnvKeyWrap + `.

The KMS key the files are wrapped with comes from ` + config.EnvKeyWrap + `, never from the
data directory: a file wrapped with another key is refused. To use another key,
name it with --from <provider>:<key URI> and confirm it with --confirm-from.

Each change is published as the security.key_wrapping_changed event and recorded in
the audit log at the next start.
`

// runKey runs "mongorescue key". It returns the exit code.
func runKey(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, keyUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub := args[0]
	fs := flag.NewFlagSet("mongorescue key "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "Data directory (env "+config.EnvDataDir+", default "+config.DefaultDataDir+")")
	var to *string
	var confirm *bool
	from := new(string)
	confirmFrom := new(bool)
	switch sub {
	case "status":
	case "rewrap":
		to = fs.String("to", "", "The new KMS key, <provider>:<key URI> (required)")
	case "unwrap":
		confirm = fs.Bool("confirm-plaintext", false, "Confirm that secret.key is written to the data directory in plain form")
	default:
		fmt.Fprintf(stderr, "unknown key command %q\n\n%s", sub, keyUsage)
		return 2
	}
	if sub != "status" {
		from = fs.String("from", "", "The KMS key the files are wrapped with, <provider>:<key URI>, instead of "+config.EnvKeyWrap+" (needs --confirm-from)")
		confirmFrom = fs.Bool("confirm-from", false, "Confirm that the KMS key of --from is used with this instance's credentials")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = strings.TrimSpace(getenv(config.EnvDataDir))
	}
	if dir == "" {
		dir = config.DefaultDataDir
	}
	dir = filepath.Clean(dir)

	if sub == "status" {
		return keyStatus(dir, stdout, stderr)
	}
	source, code := keySource(sub, *from, *confirmFrom, getenv, stderr)
	if code != 0 {
		return code
	}
	var spec keywrap.Spec
	if sub == "rewrap" {
		var err error
		if spec, err = keywrap.ParseSpec(*to); err != nil || !spec.Enabled() {
			fmt.Fprintln(stderr, "mongorescue key rewrap: --to must name the new KMS key as <provider>:<key URI>")
			return 2
		}
	}
	if sub == "unwrap" && !*confirm {
		fmt.Fprintln(stderr, "mongorescue key unwrap writes secret.key to the data directory in plain form, unprotected by the KMS.\n"+
			"Run it again with --confirm-plaintext to proceed.")
		return 2
	}

	lock, err := store.LockDataDir(dir)
	if err != nil {
		if errors.Is(err, store.ErrDataDirLocked) {
			fmt.Fprintln(stderr, "MongoRescue is running on "+dir+": stop it first.")
			return 1
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = lock.Release() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := keywrap.OfflineConfig{DataDir: dir, Source: source, Open: openKeyWrapper, Actor: localActor(), Retry: keyRetry}
	var change *keywrap.Change
	if sub == "rewrap" {
		change, err = keywrap.Rewrap(ctx, cfg, spec)
	} else {
		change, err = keywrap.UnwrapToFiles(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mongorescue key %s: %v\n", sub, err)
		return 1
	}
	switch {
	case sub == "rewrap" && len(change.Files) == 0:
		fmt.Fprintf(stdout, "secret.key is already wrapped with %s; nothing changed.\n", spec)
	case sub == "rewrap":
		fmt.Fprintf(stdout, "Rewrapped %s with %s.\nSet %s=%s before you start MongoRescue, and download a new recovery kit.\n"+
			"Keep the old KMS key until no backup of the data directory and no recovery kit still holds a key wrapped with it.\n",
			strings.Join(change.Files, ", "), spec, config.EnvKeyWrap, spec)
	default:
		fmt.Fprintf(stdout, "Wrote %s in plain form (mode 0600) and removed the wrapped files.\n"+
			"Unset %s before you start MongoRescue (otherwise it wraps the key again), and download a new recovery kit.\n",
			strings.Join(change.Files, ", "), config.EnvKeyWrap)
	}
	return 0
}

// keySource returns the KMS key the wrapped files must be wrapped with:
// MONGORESCUE_KEY_WRAP, or --from when it is confirmed with --confirm-from. The
// spec recorded in the data directory is never used to reach a KMS. A non-zero
// code is the exit code of a usage error.
func keySource(sub, from string, confirmed bool, getenv func(string) string, stderr io.Writer) (keywrap.Spec, int) {
	name := "mongorescue key " + sub
	if strings.TrimSpace(from) != "" {
		spec, err := keywrap.ParseSpec(from)
		if err != nil || !spec.Enabled() {
			fmt.Fprintln(stderr, name+": --from must name a KMS key as <provider>:<key URI>")
			return keywrap.Spec{}, 2
		}
		if !confirmed {
			fmt.Fprintf(stderr, "%s: --from names %s, so this instance's KMS credentials are sent there.\n"+
				"Check that it is your KMS key and run it again with --confirm-from.\n", name, spec)
			return keywrap.Spec{}, 2
		}
		return spec, 0
	}
	spec, err := keywrap.ParseSpec(getenv(config.EnvKeyWrap))
	if err != nil || !spec.Enabled() {
		fmt.Fprintln(stderr, name+": set "+config.EnvKeyWrap+" to the KMS key the files are wrapped with, "+
			"or name it with --from <provider>:<key URI> --confirm-from")
		return keywrap.Spec{}, 2
	}
	return spec, 0
}

// keyStatus prints how secret.key of dir is stored.
func keyStatus(dir string, stdout, stderr io.Writer) int {
	f, err := keywrap.ReadFile(filepath.Join(dir, keywrap.WrappedKeyFileName))
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "secret.key is wrapped by a KMS\n  provider:    %s\n  key URI:     %s\n  key ID:      %s\n  fingerprint: %s\n  wrapped at:  %s\n",
			f.Provider, f.KeyURI, f.KeyID, f.Fingerprint, f.WrappedAt.Format("2006-01-02T15:04:05Z07:00"))
		return 0
	case !errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err == nil {
		fmt.Fprintln(stdout, "secret.key is a plaintext file (key wrapping is off)")
		return 0
	}
	fmt.Fprintln(stdout, "no secret key in "+dir+" (MONGORESCUE_SECRET_KEY, or a first start)")
	return 0
}

// localActor names the operating system user running a command.
func localActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "local:" + u.Username
	}
	return "local"
}
