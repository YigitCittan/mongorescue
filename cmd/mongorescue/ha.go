package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
)

// haCommand is the command of the HA operator tools ("mongorescue ha ...").
const haCommand = "ha"

// runHA runs "mongorescue ha force-takeover": it asks the instance whose data
// directory it is given to force its next takeover past the rollback checks
// (docs/design/ha.md#forcing-a-takeover). It works on the data directory, not
// through the API: a standby serves none. It returns the process exit code.
func runHA(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprint(stderr, "Usage: mongorescue ha force-takeover (--from-snapshot KEY | --keep-local-database) [--data-dir DIR] [--cluster NAME]\n\n"+
			"Forces the next takeover of the HA instance that owns DIR past the rollback checks: it restores the named\n"+
			"metadata snapshot (its header is still verified when it has one) or keeps its own database, then signs the\n"+
			"lease again. The request is used once and recorded in the audit log. Check the HA target first.\n\n")
	}
	if len(args) == 0 || args[0] != "force-takeover" {
		usage()
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	fs := flag.NewFlagSet("mongorescue ha force-takeover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		usage()
		fs.PrintDefaults()
	}
	dataDir := config.DefaultDataDir
	if v := strings.TrimSpace(getenv(config.EnvDataDir)); v != "" {
		dataDir = v
	}
	dir := fs.String("data-dir", dataDir, "Data directory of the instance (env "+config.EnvDataDir+")")
	cluster := fs.String("cluster", strings.TrimSpace(getenv(config.EnvHACluster)), "HA cluster name (env "+config.EnvHACluster+")")
	snapshot := fs.String("from-snapshot", "", "Key of the metadata snapshot to restore, as the log names it ("+ha.SnapshotsPrefix+"<cluster>/mongorescue-<time>Z-e<epoch>.db.age)")
	keepLocal := fs.Bool("keep-local-database", false, "Keep the instance's own database instead of restoring a snapshot")
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
	o := ha.Override{Cluster: *cluster, Snapshot: strings.TrimSpace(*snapshot), KeepLocal: *keepLocal,
		RequestedBy: operatorName(getenv), RequestedAt: time.Now().UTC()}
	if err := ha.WriteOverride(filepath.Clean(*dir), o); err != nil {
		fmt.Fprintf(stderr, "mongorescue ha force-takeover: %v\n", err)
		if errors.Is(err, ha.ErrInvalidOverride) {
			return 2
		}
		return 1
	}
	what := "keep its own database"
	if o.Snapshot != "" {
		what = "restore " + o.Snapshot
	}
	fmt.Fprintf(stdout, "Requested: at its next takeover, the instance in %s will %s for HA cluster %s, without the rollback checks.\n"+
		"A standby tries again within the lease TTL; the forced takeover is logged and recorded in the audit log.\n",
		filepath.Clean(*dir), what, o.Cluster)
	return 0
}

// operatorName names the operator: the operating system user.
func operatorName(getenv func(string) string) string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name := getenv("USER"); name != "" {
		return name
	}
	return "unknown"
}
