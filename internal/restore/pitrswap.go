package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrSwapFailed indicates that copying a clone of an in-place point-in-time
// restore over its original database failed: the original may be partially
// replaced, and the clone is kept.
var ErrSwapFailed = errors.New("restore: replacing the original database with its clone failed")

// CollectionAdmin lists and drops the collections of a database on the target
// server (implemented by *mongoconn.Prober). The swap of an in-place point-in-time
// restore uses it to check the clones and the copies and to drop the collections
// of an original database that its clone does not have.
type CollectionAdmin interface {
	// CollectionCounts returns the collections and views of database (system
	// collections left out) with their estimated document counts.
	CollectionCounts(ctx context.Context, uri, database string) ([]models.CollectionCount, error)
	// DropCollection drops collection (or view) in database.
	DropCollection(ctx context.Context, uri, database, collection string) error
}

// WithCollectionAdmin enables in-place point-in-time restores (SwapPITRClones).
func WithCollectionAdmin(a CollectionAdmin) Option {
	return func(e *Engine) {
		e.collections = a
	}
}

// DumpRunner starts the tool name (mongodump) with args and returns its standard
// output, its standard error and its wait. stdout must be closed by the caller
// once the reading side is done.
type DumpRunner func(ctx context.Context, name string, args ...string) (stdout io.ReadCloser, stderr io.Reader, wait func() error, err error)

// WithDumpRunner overrides the runner of mongodump in SwapPITRClones (primarily
// for unit testing).
func WithDumpRunner(r DumpRunner) Option {
	return func(e *Engine) {
		e.dumper = r
	}
}

// newDumpRunner returns the default mongodump runner: it resolves the tool like
// newProcessRunner and starts it in its own process group with stdout and stderr
// piped. The stdout pipe is an *os.File, which a mongorestore started with it as
// stdin reads directly (the archive never passes through this process).
func newDumpRunner(tools *mongotools.Resolver, logger *slog.Logger) DumpRunner {
	return func(ctx context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		path, err := tools.Resolve(name)
		if err != nil {
			mongotools.LogNotFound(ctx, logger, err)
			return nil, nil, nil, err
		}
		cmd := mongotools.Command(ctx, path, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, nil, nil, err
		}
		if err := mongotools.Start(cmd); err != nil {
			return nil, nil, nil, err
		}
		return stdout, stderr, func() error { return mongotools.Wait(ctx, cmd) }, nil
	}
}

// SwapPITRClones replaces each original database of a completed in-place
// point-in-time restore (record.PITR.InPlace.Databases) with its clone
// (<db><CloneSuffix>), one database after another:
//
//  1. The clone must still list exactly what SnapshotPITRClones recorded
//     (collections, views, document counts); otherwise the original is left
//     untouched (ErrCloneUnreadable).
//  2. mongodump --archive --db=<clone> is piped straight into mongorestore
//     --archive --nsInclude=<clone>.* --nsFrom=<clone>.* --nsTo=<db>.* --drop
//     (both through the private config file of mongotools.WriteConfig, reading
//     the primary): every collection and view of the clone replaces the one of
//     the same name, with its indexes and options.
//  3. The original must then hold every collection and view of the snapshot with
//     its document count; only then are the collections and views of the
//     original that the clone does not have dropped (system collections are left
//     alone), so the original equals the clone.
//
// swapped is called with each original once it holds the restored state. Nothing
// is buffered: the archive flows through an operating system pipe. It fails with
// ErrSwapFailed naming the database; the clones are never dropped here.
func (e *Engine) SwapPITRClones(ctx context.Context, req models.RestoreRequest, record *models.RestoreRecord, swapped func(db string)) error {
	if record == nil || record.PITR == nil || record.PITR.InPlace == nil {
		return errors.New("restore: not an in-place point-in-time restore")
	}
	if e.collections == nil {
		return errors.New("restore: in-place point-in-time restores are not available in this setup")
	}
	uri := e.resolveURI(req)
	if req.MongoURI != "" {
		ctx = mongotls.NewContext(ctx, req.MongoTLS)
	}
	ctx, uri, releaseCredentials, err := e.holdCredentials(ctx, req, uri)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSwapFailed, err)
	}
	defer releaseCredentials()
	tracker := runs.FromContext(ctx)
	info := record.PITR
	toolURI := mongotools.WithReadPreference(mongotools.WithConnectionDefaults(uri), "primary", nil)
	configArgs, cleanup, err := mongotools.WriteConfig(e.configDir, toolURI, mongotls.FromContext(ctx))
	if err != nil {
		return fmt.Errorf("%w: prepare the tools config: %w", ErrSwapFailed, err)
	}
	defer cleanup()
	for _, db := range info.InPlace.Databases {
		clone := db + info.CloneSuffix
		if c := runs.CancellationOf(ctx); c != nil {
			return fmt.Errorf("%w: %s: %w", ErrSwapFailed, db, c)
		}
		snap := snapshotOf(info.InPlace, db)
		if snap == nil {
			return fmt.Errorf("%w: %s: %w: no record of what %s held after the restore; %s is untouched", ErrSwapFailed, db, ErrCloneUnreadable, clone, db)
		}
		start := time.Now()
		tracker.Printf("replacing database %s with %s", db, clone)
		if err := e.swapDatabase(ctx, uri, configArgs, snap); err != nil {
			e.logger.Error("in-place point-in-time restore: replacing a database failed",
				slog.String("restore_id", record.ID), logsafe.Attr("target_db", db), slog.String("error", redact.Text(err.Error())))
			return fmt.Errorf("%w: %s: %w", ErrSwapFailed, db, err)
		}
		tracker.Printf("database %s now holds the restored state (%.1fs)", db, time.Since(start).Seconds())
		if swapped != nil {
			swapped(db)
		}
	}
	return nil
}

// SnapshotPITRClones records, right after the restore into the clones of an
// in-place point-in-time restore completed, what each clone holds: its
// collections and views and their document counts
// (record.PITR.InPlace.Snapshots). The swap later copies a clone only while it
// still lists exactly that, and drops collections of an original only once the
// copy verifiably holds all of it. A clone that cannot be listed, or lists no
// collection, fails with ErrCloneUnreadable: an empty listing may come from
// missing privileges as well as from a database that did not exist at the target
// time, and an in-place restore never empties an original database.
func (e *Engine) SnapshotPITRClones(ctx context.Context, req models.RestoreRequest, record *models.RestoreRecord) error {
	if record == nil || record.PITR == nil || record.PITR.InPlace == nil {
		return errors.New("restore: not an in-place point-in-time restore")
	}
	if e.collections == nil {
		return errors.New("restore: in-place point-in-time restores are not available in this setup")
	}
	uri := e.resolveURI(req)
	if req.MongoURI != "" {
		ctx = mongotls.NewContext(ctx, req.MongoTLS)
	}
	info := record.PITR
	snaps := make([]models.PITRCloneSnapshot, 0, len(info.InPlace.Databases))
	for _, db := range info.InPlace.Databases {
		clone := db + info.CloneSuffix
		colls, err := e.collections.CollectionCounts(ctx, uri, clone)
		if err != nil {
			return fmt.Errorf("%w: list %s: %w", ErrCloneUnreadable, clone, err)
		}
		if len(colls) == 0 {
			return fmt.Errorf("%w: %s lists no collection, so replacing %s would empty it; an in-place restore never empties an original database "+
				"(if %s did not exist at the target time, drop it yourself; if the connection's user may not list every collection, grant it)",
				ErrCloneUnreadable, clone, db, db)
		}
		snaps = append(snaps, models.PITRCloneSnapshot{Database: db, Clone: clone, Collections: colls})
	}
	info.InPlace.Snapshots = snaps
	return nil
}

// ErrCloneUnreadable indicates that a clone of an in-place point-in-time restore
// cannot be read completely (it lists no collection, or not what it held after
// the restore into it): its original is left untouched.
var ErrCloneUnreadable = errors.New("restore: the clone of an in-place point-in-time restore cannot be read completely")

// snapshotOf returns the snapshot of original database db, or nil.
func snapshotOf(ip *models.PITRInPlace, db string) *models.PITRCloneSnapshot {
	for i := range ip.Snapshots {
		if ip.Snapshots[i].Database == db {
			return &ip.Snapshots[i]
		}
	}
	return nil
}

// collectionDiff describes how got differs from want (empty when it holds every
// collection of want with the same kind and document count); extra collections
// of got are ignored unless exact.
func collectionDiff(want, got []models.CollectionCount, exact bool) string {
	byName := make(map[string]models.CollectionCount, len(got))
	for _, c := range got {
		byName[c.Name] = c
	}
	var diffs []string
	for _, w := range want {
		g, ok := byName[w.Name]
		switch {
		case !ok:
			diffs = append(diffs, w.Name+" is missing")
		case g.View != w.View:
			diffs = append(diffs, w.Name+" changed kind")
		case !w.View && g.Documents != w.Documents:
			diffs = append(diffs, fmt.Sprintf("%s has %d documents, not %d", w.Name, g.Documents, w.Documents))
		}
		delete(byName, w.Name)
	}
	if exact {
		for name := range byName {
			diffs = append(diffs, name+" is new")
		}
	}
	slices.Sort(diffs)
	return strings.Join(diffs, ", ")
}

// swapDatabase copies clone over db (see SwapPITRClones). Nothing of db is
// dropped unless the clone still lists exactly its snapshot and the copy holds
// every collection of it with its document count.
func (e *Engine) swapDatabase(ctx context.Context, uri string, configArgs []string, snap *models.PITRCloneSnapshot) error {
	db, clone := snap.Database, snap.Clone
	cloneColls, err := e.collections.CollectionCounts(ctx, uri, clone)
	if err != nil {
		return fmt.Errorf("%w: list %s: %w; %s is untouched", ErrCloneUnreadable, clone, err, db)
	}
	if diff := collectionDiff(snap.Collections, cloneColls, true); diff != "" || len(cloneColls) == 0 {
		return fmt.Errorf("%w: %s does not list what it held after the restore (%s); %s is untouched", ErrCloneUnreadable, clone, diff, db)
	}
	if err = e.copyDatabase(ctx, uri, configArgs, db, clone); err != nil {
		return err
	}
	origColls, err := e.collections.CollectionCounts(ctx, uri, db)
	if err != nil {
		return fmt.Errorf("list %s after the copy: %w; nothing was dropped", db, err)
	}
	if diff := collectionDiff(snap.Collections, origColls, false); diff != "" {
		return fmt.Errorf("the copy into %s is incomplete (%s); nothing was dropped", db, diff)
	}
	for _, c := range origColls {
		if slices.ContainsFunc(snap.Collections, func(s models.CollectionCount) bool { return s.Name == c.Name }) {
			continue
		}
		if err := e.collections.DropCollection(ctx, uri, db, c.Name); err != nil {
			return fmt.Errorf("drop %s.%s, which the restored state does not have: %w", db, c.Name, err)
		}
		runs.FromContext(ctx).Printf("dropped %s.%s, which did not exist at the target time", db, c.Name)
	}
	return nil
}

// copyDatabase pipes mongodump of clone into mongorestore --drop renamed to db.
func (e *Engine) copyDatabase(ctx context.Context, uri string, configArgs []string, db, clone string) error {
	dumpArgs := append([]string{configArgs[0], "--archive", "--db=" + clone}, configArgs[1:]...)
	stdout, dumpStderr, dumpWait, err := e.dumper(ctx, "mongodump", dumpArgs...)
	if err != nil {
		return fmt.Errorf("start mongodump: %w", err)
	}
	// mongodump's stderr is drained while it runs, so it never blocks on a full
	// pipe; the goroutine ends when mongodump exits.
	dumpTail := &mongotools.TailBuffer{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(dumpTail, dumpStderr)
	}()
	src := escapeNamespace(clone)
	args := append([]string{configArgs[0], "--archive", "--nsInclude=" + src + ".*",
		"--nsFrom=" + src + ".*", "--nsTo=" + escapeNamespace(db) + ".*", "--drop"}, configArgs[1:]...)
	if e.bypassValidation(ctx, uri, db) {
		args = append(args, "--bypassDocumentValidation")
	}
	stderr, waitErr, startErr := e.runRestoreTool(ctx, stdout, args)
	// Closing the reading end stops a mongodump whose reader is gone (EPIPE).
	_ = stdout.Close()
	<-drained
	dumpErr := dumpWait()
	switch {
	case startErr != nil:
		return startErr
	case ctx.Err() != nil && (waitErr != nil || dumpErr != nil):
		return fmt.Errorf("aborted: %w", ctx.Err())
	case waitErr != nil:
		return fmt.Errorf("mongorestore failed: %w (stderr: %s)", waitErr, stderrTail(stderr))
	case dumpErr != nil:
		return fmt.Errorf("mongodump of %s failed: %w (stderr: %s)", clone, dumpErr, stderrTail(dumpTail.String()))
	}
	if failed, _ := failedDocuments(stderr); failed > 0 {
		return fmt.Errorf("%w: %d document(s) failed to restore into %s; mongorestore output: %s", ErrDocumentsFailed, failed, db, stderrTail(stderr))
	}
	return nil
}
