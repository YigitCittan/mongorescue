package restore

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// ErrNoArtifact indicates that a backup record has no stored artifact to read (a
// backup that failed before writing one, or a pruned backup).
var ErrNoArtifact = errors.New("restore: backup has no stored artifact")

// ArchivePreludeLimit bounds the decompressed bytes ArchiveCollections reads.
const ArchivePreludeLimit = mongotools.DefaultPreludeLimit

// ArchiveCollections lists the collections stored in the archive of backup rec by
// reading only the archive prelude: the artifact is streamed from its storage target,
// decrypted (age) and decompressed (gzip) on the fly, and the stream is closed as soon
// as the prelude ends, so the data of the backup is never downloaded. At most
// ArchivePreludeLimit decompressed bytes are read; ctx bounds the whole read.
//
// Errors: ErrNoArtifact for a record without a storage key, an error wrapping
// encryption.ErrEncryptionKeyRequired (with KeyRequiredHint) for an encrypted backup
// without key material, encryption.ErrDecryptionFailed for a wrong key, and the
// mongotools prelude errors (ErrNotArchive, ErrArchiveTruncated, ErrPreludeTooLarge,
// ErrMalformedPrelude) for an artifact whose prelude cannot be read.
func (e *Engine) ArchiveCollections(ctx context.Context, rec *models.BackupRecord) ([]models.BackupCollection, error) {
	if e == nil {
		return nil, errors.New("restore: no restore engine configured")
	}
	if rec == nil {
		return nil, errors.New("restore: backup record must be provided")
	}
	if strings.TrimSpace(rec.StorageKey) == "" {
		return nil, ErrNoArtifact
	}
	run, err := e.forRun(ctx, rec.StorageTargetID)
	if err != nil {
		return nil, err
	}
	encrypted := rec.Encrypted || keyEncrypted(rec.StorageKey)
	if encrypted && run.decryptor == nil {
		return nil, fmt.Errorf("%w: %s", encryption.ErrEncryptionKeyRequired, KeyRequiredHint)
	}

	stream, err := storage.RetrieveVersion(ctx, run.storage, rec.StorageKey, rec.StorageVersionID)
	if err != nil {
		return nil, fmt.Errorf("retrieve backup stream: %w", err)
	}
	// Closing the stream early abandons the rest of the object (an S3 GET included).
	defer stream.Close()

	stored := bufio.NewReader(&ctxReader{ctx: ctx, r: stream})
	if !encrypted {
		if encrypted, err = run.detectUnrecordedEncryption(stored, rec); err != nil {
			return nil, err
		}
	}
	var plain io.Reader = stored
	if encrypted {
		decrypted, decErr := run.decryptor.Decrypt(stored)
		if decErr != nil {
			return nil, fmt.Errorf("decrypt backup stream: %w", decErr)
		}
		plain = decrypted
	}
	archive := bufio.NewReader(plain)
	isGzip, err := sniffGzip(archive, rec.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("read backup stream: %w", err)
	}
	var r io.Reader = archive
	if isGzip {
		zr, zErr := gzip.NewReader(archive)
		if zErr != nil {
			return nil, fmt.Errorf("decompress backup stream: %w", mapGzipError(zErr))
		}
		defer zr.Close()
		r = zr
	}

	prelude, err := mongotools.ReadArchivePrelude(r, ArchivePreludeLimit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read archive prelude: %w", context.Cause(ctx))
		}
		return nil, fmt.Errorf("read archive prelude: %w", mapGzipError(err))
	}
	out := make([]models.BackupCollection, 0, len(prelude.Collections))
	for _, c := range prelude.Collections {
		// A database backup holds only that database; anything else is not restorable
		// through the restore of this backup.
		if rec.Database != "" && c.Database != "" && c.Database != rec.Database {
			continue
		}
		out = append(out, models.BackupCollection{Name: c.Name, Type: c.Type, ViewOn: c.ViewOn, SizeBytes: c.SizeBytes})
	}
	// mongodump writes the prelude in no particular order.
	slices.SortFunc(out, func(a, b models.BackupCollection) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// mapGzipError reports a gzip stream that ends early as a truncated prelude.
func mapGzipError(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %w", mongotools.ErrArchiveTruncated, err)
	}
	return err
}
