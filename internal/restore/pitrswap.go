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
// restore uses it to drop the collections of an original database that its clone
// does not have.
type CollectionAdmin interface {
	// CollectionNames returns the names of the collections and views of database.
	CollectionNames(ctx context.Context, uri, database string) ([]string, error)
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
//  1. mongodump --archive --db=<clone> is piped straight into mongorestore
//     --archive --nsInclude=<clone>.* --nsFrom=<clone>.* --nsTo=<db>.* --drop
//     (both through the private config file of mongotools.WriteConfig, reading
//     the primary): every collection and view of the clone replaces the one of
//     the same name, with its indexes and options.
//  2. The collections and views of the original that the clone does not have
//     are dropped (system collections are left alone), so the original equals
//     the clone. A clone that does not exist (the database did not exist at the
//     target time) leaves the original without collections.
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
		start := time.Now()
		tracker.Printf("replacing database %s with %s", db, clone)
		if err := e.swapDatabase(ctx, uri, configArgs, db, clone); err != nil {
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

// swapDatabase copies clone over db (see SwapPITRClones).
func (e *Engine) swapDatabase(ctx context.Context, uri string, configArgs []string, db, clone string) error {
	cloneColls, err := e.collections.CollectionNames(ctx, uri, clone)
	if err != nil {
		return fmt.Errorf("list the collections of %s: %w", clone, err)
	}
	if len(cloneColls) > 0 {
		if err = e.copyDatabase(ctx, uri, configArgs, db, clone); err != nil {
			return err
		}
	}
	origColls, err := e.collections.CollectionNames(ctx, uri, db)
	if err != nil {
		return fmt.Errorf("list the collections of %s: %w", db, err)
	}
	for _, name := range origColls {
		if strings.HasPrefix(name, "system.") || slices.Contains(cloneColls, name) {
			continue
		}
		if err := e.collections.DropCollection(ctx, uri, db, name); err != nil {
			return fmt.Errorf("drop %s.%s, which the restored state does not have: %w", db, name, err)
		}
		runs.FromContext(ctx).Printf("dropped %s.%s, which did not exist at the target time", db, name)
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
