package collector

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// cleanupTimeout bounds the removal of an object that was not committed.
const cleanupTimeout = 30 * time.Second

// errConsumerStopped unblocks the producer when the upload stopped reading.
var errConsumerStopped = errors.New("collector: the chunk upload stopped reading")

// writeChunk reads rng into a new chunk object of chain and commits it. The data
// flows driver -> pipe -> scanner -> gzip -> age -> SHA-256 -> Storage.Save without
// being buffered beyond one entry. Nothing is committed and the object is removed
// when any stage fails. fromTerm is the term of the entry at rng.From, recorded on
// an empty chunk. It returns the chunk and the uncompressed bytes read.
func (w *worker) writeChunk(ctx context.Context, chainID string, rng pitr.OplogRange, fromTerm int64) (*pitr.Chunk, int64, error) {
	enc := w.svc.cfg.Encryptor()
	if enc == nil {
		return nil, 0, ErrEncryptionRequired
	}
	driver, err := w.svc.cfg.Storage(ctx, w.stream.TargetID)
	if err != nil {
		return nil, 0, fmt.Errorf("storage target: %w", err)
	}
	key := ChunkKey(w.stream, chainID, rng.From, rng.To)

	pr, pw := io.Pipe()
	var (
		stats   pitr.OplogStats
		scanner *oplog.Scanner
		raw     int64
	)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	done := make(chan error, 1)
	go func() {
		err := func() error {
			ew, err := enc.Encrypt(pw)
			if err != nil {
				return fmt.Errorf("encrypt the chunk: %w", err)
			}
			gz := gzip.NewWriter(ew)
			counted := &countWriter{w: gz}
			scanner = oplog.NewScanner(counted)
			stats, err = w.m.ReadOplog(readCtx, rng, scanner)
			if err != nil {
				return err
			}
			if err := scanner.Finish(); err != nil {
				return fmt.Errorf("walk the chunk: %w", err)
			}
			raw = counted.n
			// The age stream is sealed only after a complete read, so a failed read
			// never leaves a well-formed but truncated object.
			if err := gz.Close(); err != nil {
				return fmt.Errorf("compress the chunk: %w", err)
			}
			if err := ew.Close(); err != nil {
				return fmt.Errorf("seal the chunk: %w", err)
			}
			return nil
		}()
		_ = pw.CloseWithError(err)
		done <- err
	}()

	h := sha256.New()
	counted := &hashReader{r: pr, h: h}
	saved, saveErr := driver.Save(ctx, key, counted)
	if saveErr != nil {
		cancelRead()
	}
	_ = pr.CloseWithError(errConsumerStopped)
	readErr := <-done

	switch {
	case readErr != nil && !errors.Is(readErr, errConsumerStopped):
		w.removeObject(ctx, driver, key)
		return nil, 0, readErr
	case saveErr != nil:
		w.removeObject(ctx, driver, key)
		return nil, 0, fmt.Errorf("store the chunk: %w", saveErr)
	case readErr != nil:
		w.removeObject(ctx, driver, key)
		return nil, 0, readErr
	}
	if err := checkScan(scanner, stats); err != nil {
		w.removeObject(ctx, driver, key)
		return nil, 0, err
	}

	c := &pitr.Chunk{
		ID: newID("chk_"), StreamID: w.stream.ID, ChainID: chainID, TargetID: w.stream.TargetID, StorageKey: key,
		From: rng.From, To: rng.To, FirstTerm: fromTerm, LastTerm: fromTerm, Entries: int64(stats.Entries),
		SizeBytes: counted.n, SHA256: hex.EncodeToString(h.Sum(nil)), Encrypted: true, EncryptionMode: string(enc.Mode()),
		CreatedAt: w.svc.now(),
	}
	if stats.Entries > 0 {
		c.FirstTerm, c.LastTerm = stats.First.Term, stats.Last.Term
	}
	if saved != nil {
		c.VersionID, c.RetainUntil = saved.VersionID, saved.RetainUntil
	}
	if err := w.svc.cfg.Repo.CommitChunk(ctx, c); err != nil {
		if w.svc.cfg.OnWriteError != nil {
			w.svc.cfg.OnWriteError(ctx, err)
		}
		w.removeObject(ctx, driver, key)
		return nil, 0, fmt.Errorf("commit the chunk: %w", err)
	}
	return c, raw, nil
}

// checkScan compares the scanner's walk of the written bytes with the read's own
// statistics.
func checkScan(sc *oplog.Scanner, stats pitr.OplogStats) error {
	if sc == nil {
		return errors.New("the chunk was not walked")
	}
	if sc.Count() != int64(stats.Entries) {
		return fmt.Errorf("the chunk holds %d entries, the read reported %d", sc.Count(), stats.Entries)
	}
	if stats.Entries == 0 {
		return nil
	}
	first, last := sc.First(), sc.Last()
	if first.TS.T != stats.First.TS.T || first.TS.I != stats.First.TS.I || last.TS.T != stats.Last.TS.T || last.TS.I != stats.Last.TS.I {
		return fmt.Errorf("the chunk spans %d.%d-%d.%d, the read reported %s-%s",
			first.TS.T, first.TS.I, last.TS.T, last.TS.I, stats.First.TS, stats.Last.TS)
	}
	return nil
}

// removeObject deletes an object that was not committed, detached from ctx's
// cancellation. On a locked target the object cannot go before its lock ends: it is
// left without a delete marker, so the orphan purge finds and deletes it then.
func (w *worker) removeObject(ctx context.Context, driver storage.Storage, key string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if _, err := storage.Purge(cctx, driver, key, "", w.svc.now()); err != nil && !errors.Is(err, storage.ErrNotFound) {
		w.svc.logger.Warn("cannot remove an uncommitted oplog chunk", logsafe.Attr("stream_id", w.stream.ID),
			logsafe.Attr("storage_key", key), logsafe.Error(err))
	}
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// hashReader hashes and counts the bytes read through it.
type hashReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (r *hashReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.h.Write(p[:n])
		r.n += int64(n)
	}
	return n, err
}
