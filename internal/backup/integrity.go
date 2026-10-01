package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/verify"
)

// ErrChecksumMismatch indicates that the archive read back from storage after the
// upload did not match the checksum computed while it was written; the backup is
// failed and the damaged artifact deleted. It aliases verify.ErrChecksumMismatch.
var ErrChecksumMismatch = verify.ErrChecksumMismatch

// ManifestFunc returns the manifest (document counts and index specifications of
// every collection) of database on the server at uri. Implementations must never
// include the URI's credentials in errors.
type ManifestFunc func(ctx context.Context, uri, database string) (*models.Manifest, error)

// manifestTimeout bounds each manifest capture.
const manifestTimeout = 2 * time.Minute

// WithManifestCapturer makes every backup capture a manifest of the dumped
// collections with fn: once before the dump and once after it, so each collection
// records the range its document count moved in while it was dumped. A capture that
// fails only logs a warning; the backup goes on without a manifest.
func WithManifestCapturer(fn ManifestFunc) Option {
	return func(e *Engine) {
		e.manifest = fn
	}
}

// WithVerifyAfterUpload makes every backup re-read its stored archive after the
// upload and compare it with the checksum computed while it was written (see
// RunConfig.Verify). It applies when no WithRunConfig is set.
func WithVerifyAfterUpload(on bool) Option {
	return func(e *Engine) {
		e.verifyUpload = on
	}
}

// captureManifest returns the manifest of the collections opts dumps, or nil when
// no capturer is configured or the capture failed.
func (e *Engine) captureManifest(ctx context.Context, uri string, opts models.BackupOptions) *models.Manifest {
	if e.manifest == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()
	m, err := e.manifest(ctx, uri, opts.Database)
	if err != nil || m == nil {
		if err != nil {
			e.logger.Warn("could not capture the backup manifest; restore tests of this backup will not compare counts and indexes",
				slog.String("database", opts.Database), slog.String("error", redact.Text(err.Error())))
		}
		return nil
	}
	m.Collections = slices.DeleteFunc(m.Collections, func(c models.CollectionManifest) bool {
		return !dumped(c.Name, opts)
	})
	m.Normalize()
	return m
}

// dumped reports whether mongodump with opts includes collection name.
func dumped(name string, opts models.BackupOptions) bool {
	if include := trimmedNames(opts.Collections); len(include) > 0 {
		return slices.Contains(include, name)
	}
	return !slices.Contains(trimmedNames(opts.ExcludeCollections), name)
}

// finishManifest widens the manifest captured before the dump with the counts after
// it and attaches it to record.
func (e *Engine) finishManifest(ctx context.Context, uri string, opts models.BackupOptions, before *models.Manifest, record *models.BackupRecord) {
	if before == nil {
		return
	}
	if after := e.captureManifest(ctx, uri, opts); after != nil {
		before.MergeCounts(after)
	}
	record.Manifest, record.HasManifest = before, true
}

// shouldVerify resolves the post-upload verification of a run with opts.
func (e *Engine) shouldVerify(opts models.BackupOptions) bool {
	return opts.Verify.Resolve(e.verifyUpload)
}

// verifyAfterUpload re-reads the stored archive of a completed backup and records
// the outcome on record. A mismatch deletes the artifact and returns an error
// wrapping ErrChecksumMismatch (the caller fails the backup); a verification that
// could not run (storage error, cancellation) is recorded as such and the backup
// stays completed.
func (e *Engine) verifyAfterUpload(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) error {
	if !e.shouldVerify(opts) {
		return nil
	}
	var dec *encryption.Decryptor
	if record.Encrypted {
		dec = e.verifyDecryptor
	}
	res := verify.Archive(ctx, e.storage, record, verify.Options{Decryptor: dec})
	res.Apply(record)
	switch res.Status {
	case models.VerificationOK:
		e.logger.Info("backup archive verified after upload",
			slog.String("backup_id", record.ID), slog.Int64("bytes", res.Bytes), slog.Bool("decrypted", res.Decrypted))
		return nil
	case models.VerificationMismatch:
		e.logger.Error("backup archive does not match its checksum after upload; deleting it",
			slog.String("backup_id", record.ID), slog.String("error", redact.Text(res.Err.Error())))
		e.deleteArtifact(ctx, record.StorageKey)
		if errors.Is(res.Err, ErrChecksumMismatch) {
			return fmt.Errorf("verify after upload: %w", res.Err)
		}
		return fmt.Errorf("verify after upload: %w: %w", ErrChecksumMismatch, res.Err)
	default:
		e.logger.Warn("backup archive could not be verified after upload",
			slog.String("backup_id", record.ID), slog.String("error", redact.Text(res.Err.Error())))
		return nil
	}
}
