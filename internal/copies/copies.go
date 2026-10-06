// Package copies copies backup archives to further storage targets (3-2-1
// copies): the copy pipeline, which streams an archive from its primary target into
// a copy target and checks it against the primary's SHA-256 on the way, and the
// persistent copy queue that retries copies with backoff.
package copies

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/throttle"
)

// Sentinel errors.
var (
	// ErrChecksumMismatch means the bytes read from the primary archive do not match
	// the SHA-256 (or size) recorded for the backup: the copy is refused before
	// the object is completed on the copy target.
	ErrChecksumMismatch = errors.New("copies: the archive read from the primary target does not match its recorded checksum")
	// ErrNoChecksum means the backup has no checksum to check a copy against.
	ErrNoChecksum = errors.New("copies: the backup has no checksum to check a copy against")
	// ErrIncomplete means the copy target stored the object without reading the
	// archive to its end, so the checksum could not be checked.
	ErrIncomplete = errors.New("copies: the copy target did not read the whole archive")
)

// StorageFunc returns the driver of a storage target (implemented by
// targets.Service.Storage).
type StorageFunc func(ctx context.Context, targetID string) (storage.Storage, error)

// Copy streams the archive of rec (rec.StorageKey, version rec.StorageVersionID)
// from src into dst under key, at most at bytesPerSecond (0 = unlimited). The bytes
// are hashed on the way: when they do not match rec.SHA256 and rec.SizeBytes, the
// stream fails at its end with ErrChecksumMismatch, so dst never completes the
// object (local targets write a temporary file, S3 aborts the upload). Nothing is
// buffered beyond the drivers' own part buffers. It returns the stored object (its
// S3 version and Object Lock retention on a locked target).
func Copy(ctx context.Context, src storage.Storage, rec *models.BackupRecord, dst storage.Storage, key string, bytesPerSecond float64) (*models.StorageObject, error) {
	want := strings.ToLower(strings.TrimSpace(rec.SHA256))
	if want == "" {
		return nil, ErrNoChecksum
	}
	stream, err := storage.RetrieveVersion(ctx, src, rec.StorageKey, rec.StorageVersionID)
	if err != nil {
		return nil, fmt.Errorf("read the primary archive %s: %w", rec.StorageKey, err)
	}
	defer func() { _ = stream.Close() }()
	vr := &verifyingReader{r: stream, h: sha256.New(), want: want, size: rec.SizeBytes}
	var in io.Reader = vr
	if bytesPerSecond > 0 {
		in = throttle.NewReader(ctx, vr, bytesPerSecond)
	}
	obj, err := dst.Save(ctx, key, in)
	switch {
	case vr.err != nil:
		return nil, vr.err
	case err != nil:
		return nil, fmt.Errorf("write the copy %s: %w", key, err)
	case !vr.checked:
		return obj, ErrIncomplete
	}
	return obj, nil
}

// verifyingReader hashes and counts what it reads and, at the end of the stream,
// returns ErrChecksumMismatch instead of io.EOF when the bytes differ from want.
type verifyingReader struct {
	r       io.Reader
	h       hash.Hash
	want    string
	size    int64
	n       int64
	checked bool
	err     error
}

// Read implements io.Reader.
func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
		v.n += int64(n)
	}
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	if v.size > 0 && v.n != v.size {
		v.err = fmt.Errorf("%w: recorded %d bytes, read %d", ErrChecksumMismatch, v.size, v.n)
		return n, v.err
	}
	if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
		v.err = fmt.Errorf("%w: recorded sha256 %s, read %s", ErrChecksumMismatch, v.want, got)
		return n, v.err
	}
	v.checked = true
	return n, io.EOF
}
