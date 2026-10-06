// Package verify re-reads a stored backup archive from its storage target and
// compares it with the SHA-256 and size recorded while it was written. With a
// decryptor it also authenticates an age-encrypted archive to its final chunk.
//
// Verification streams: the archive passes once through the hash (and the age
// reader) into io.Discard, so memory stays constant whatever the archive size, and
// nothing is written anywhere. A read rate limit keeps background sweeps from
// saturating a link or running up egress bursts.
package verify

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Sentinel errors.
var (
	// ErrChecksumMismatch means the stored archive differs from the checksum or size
	// recorded when it was written: it is damaged or was replaced.
	ErrChecksumMismatch = errors.New("verify: stored archive does not match its recorded checksum")

	// ErrNoChecksum means the record carries no checksum to compare with (records
	// written by old releases, or imported ones that were never hashed).
	ErrNoChecksum = errors.New("verify: backup record has no checksum to verify against")

	// ErrDecryptCheck means the archive matched its checksum but the configured keys
	// could not decrypt it: the archive is intact, the key material is not.
	ErrDecryptCheck = errors.New("verify: archive intact but the configured keys cannot decrypt it")
)

// ageHeader starts every age-encrypted archive.
const ageHeader = "age-encryption.org/v1"

// Options tune one verification.
type Options struct {
	// Decryptor, when set, decrypts encrypted archives to their end, which also
	// proves that the configured keys still open them.
	Decryptor *encryption.Decryptor
	// BytesPerSecond caps the read rate (0 = unlimited).
	BytesPerSecond int64
}

// Result is the outcome of one verification.
type Result struct {
	// Status is ok, mismatch or error.
	Status models.VerificationStatus
	// At is when the verification finished.
	At time.Time
	// Bytes is how many stored bytes were read.
	Bytes int64
	// Decrypted reports that the age stream was decrypted to its end.
	Decrypted bool
	// Err explains a mismatch (wrapping ErrChecksumMismatch) or an error.
	Err error
}

// Apply records r on rec (Verification, VerifiedAt, VerificationError).
func (r Result) Apply(rec *models.BackupRecord) {
	at := r.At
	rec.VerifiedAt = &at
	rec.Verification = r.Status
	rec.VerificationError = ""
	if r.Err != nil {
		rec.VerificationError = redact.Text(r.Err.Error())
	}
}

// Archive re-reads the archive of rec from st and compares it with rec.SHA256 and
// rec.SizeBytes. A difference yields VerificationMismatch with an error wrapping
// ErrChecksumMismatch; a read failure, a cancellation or a missing checksum yields
// VerificationError. When opts.Decryptor is set and the archive is encrypted, it is
// decrypted to its end as well; a matching archive the keys cannot open yields
// VerificationError wrapping ErrDecryptCheck.
func Archive(ctx context.Context, st storage.Storage, rec *models.BackupRecord, opts Options) Result {
	res := verifyArchive(ctx, st, rec, opts)
	res.At = time.Now().UTC()
	return res
}

func verifyArchive(ctx context.Context, st storage.Storage, rec *models.BackupRecord, opts Options) Result {
	expected := strings.ToLower(strings.TrimSpace(rec.SHA256))
	if expected == "" {
		return Result{Status: models.VerificationError, Err: ErrNoChecksum}
	}
	if st == nil {
		return Result{Status: models.VerificationError, Err: errors.New("verify: no storage driver")}
	}
	stream, err := storage.RetrieveVersion(ctx, st, rec.StorageKey, rec.StorageVersionID)
	if err != nil {
		return Result{Status: models.VerificationError, Err: fmt.Errorf("retrieve archive %s: %w", rec.StorageKey, err)}
	}
	defer stream.Close()

	h := sha256.New()
	counted := &countReader{r: &ctxReader{ctx: ctx, r: throttle(ctx, stream, opts.BytesPerSecond)}}
	// Every stored byte passes the hash exactly once; the buffer only allows peeking.
	stored := bufio.NewReader(io.TeeReader(counted, h))

	var decryptErr error
	decrypted := false
	if opts.Decryptor != nil && isEncrypted(rec, stored) {
		plain, err := opts.Decryptor.Decrypt(stored)
		if err == nil {
			// age only reports io.EOF after authenticating the final chunk.
			_, err = io.Copy(io.Discard, plain)
		}
		if err != nil {
			decryptErr = err
		} else {
			decrypted = true
		}
	}
	// Hash the rest (the whole object for plaintext archives or without a decryptor).
	if _, err := io.Copy(io.Discard, stored); err != nil {
		return Result{Status: models.VerificationError, Bytes: counted.n, Err: fmt.Errorf("read archive %s: %w", rec.StorageKey, err)}
	}

	if rec.SizeBytes > 0 && counted.n != rec.SizeBytes {
		return Result{Status: models.VerificationMismatch, Bytes: counted.n,
			Err: fmt.Errorf("%w: recorded %d bytes, stored archive has %d", ErrChecksumMismatch, rec.SizeBytes, counted.n)}
	}
	if actual := hex.EncodeToString(h.Sum(nil)); actual != expected {
		return Result{Status: models.VerificationMismatch, Bytes: counted.n,
			Err: fmt.Errorf("%w: recorded sha256 %s, stored archive %s", ErrChecksumMismatch, expected, actual)}
	}
	if decryptErr != nil {
		if ctx.Err() != nil {
			return Result{Status: models.VerificationError, Bytes: counted.n, Err: fmt.Errorf("verification cancelled: %w", ctx.Err())}
		}
		return Result{Status: models.VerificationError, Bytes: counted.n, Err: fmt.Errorf("%w: %w", ErrDecryptCheck, decryptErr)}
	}
	return Result{Status: models.VerificationOK, Bytes: counted.n, Decrypted: decrypted}
}

// isEncrypted reports whether the archive is age-encrypted: by its record, its key
// suffix or its header.
func isEncrypted(rec *models.BackupRecord, stored *bufio.Reader) bool {
	if rec.Encrypted || strings.HasSuffix(rec.StorageKey, encryption.FileExtension) {
		return true
	}
	head, _ := stored.Peek(len(ageHeader))
	return string(head) == ageHeader
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ctxReader fails reads once ctx is done, so a cancelled verification stops between
// reads even when the storage driver ignores the context.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// throttle returns r limited to rate bytes per second (r itself for rate <= 0).
func throttle(ctx context.Context, r io.Reader, rate int64) io.Reader {
	if rate <= 0 {
		return r
	}
	return &rateReader{ctx: ctx, r: r, rate: rate, start: time.Now()}
}

// rateReader paces reads so that the average rate since the first read never
// exceeds rate. Reads are capped at one second's worth of bytes, so a single large
// read cannot burst far ahead.
type rateReader struct {
	ctx   context.Context
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (t *rateReader) Read(p []byte) (int, error) {
	if int64(len(p)) > t.rate {
		p = p[:t.rate]
	}
	n, err := t.r.Read(p)
	t.n += int64(n)
	due := t.start.Add(time.Duration(float64(t.n) / float64(t.rate) * float64(time.Second)))
	if wait := time.Until(due); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			if err == nil {
				err = t.ctx.Err()
			}
		case <-timer.C:
		}
	}
	return n, err
}
