package collector

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/oplog"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Chunk verification limits.
const (
	// ReverifyAfter is how long a verified chunk is trusted before a sweep checks
	// it again.
	ReverifyAfter = 7 * 24 * time.Hour
	// maxSweepChunks bounds the chunks one sweep verifies.
	maxSweepChunks = 20000
)

// ErrChunkMismatch is returned by VerifyChunk for a chunk whose stored object does
// not match its record.
var ErrChunkMismatch = errors.New("oplog chunk does not match its record")

// ChunkSweep is the outcome of the chunk part of an integrity sweep.
type ChunkSweep struct {
	// Verified counts the chunks checked, Failed those that did not match.
	Verified int `json:"verified"`
	Failed   int `json:"failed"`
	// Breaks counts the places where a chunk does not start where the previous
	// one of its chain ends.
	Breaks int `json:"breaks"`
}

// VerifyChunk re-reads the object of c: its SHA-256 and size, and, with a
// decryptor, its decrypted and gunzipped entries, whose count, first and last
// positions and terms must match the record. It returns an error wrapping
// ErrChunkMismatch for a mismatch.
func (s *Service) VerifyChunk(ctx context.Context, c *pitr.Chunk) error {
	driver, err := s.cfg.Storage(ctx, c.TargetID)
	if err != nil {
		return fmt.Errorf("storage target: %w", err)
	}
	obj, err := driver.Retrieve(ctx, c.StorageKey)
	if err != nil {
		return fmt.Errorf("read the chunk object: %w", err)
	}
	defer func() { _ = obj.Close() }()
	h := sha256.New()
	counted := &hashReader{r: obj, h: h}
	var walkErr error
	sc := oplog.NewScanner(nil)
	if dec := s.decryptor(); dec != nil && c.Encrypted {
		walkErr = walkChunk(dec.Decrypt, counted, sc)
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return fmt.Errorf("read the chunk object: %w", err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != c.SHA256 || counted.n != c.SizeBytes {
		return fmt.Errorf("%w: sha256 %s of %d bytes, recorded %s of %d bytes", ErrChunkMismatch, sum, counted.n, c.SHA256, c.SizeBytes)
	}
	if walkErr != nil {
		return fmt.Errorf("%w: %w", ErrChunkMismatch, walkErr)
	}
	if s.decryptor() == nil || !c.Encrypted {
		return nil
	}
	return checkEntries(c, sc)
}

// walkChunk decrypts and gunzips r into sc.
func walkChunk(decrypt func(io.Reader) (io.Reader, error), r io.Reader, sc *oplog.Scanner) error {
	plain, err := decrypt(r)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	if _, err := io.Copy(sc, gz); err != nil {
		return fmt.Errorf("walk the entries: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	return sc.Finish()
}

// checkEntries compares the walk of a chunk with its record.
func checkEntries(c *pitr.Chunk, sc *oplog.Scanner) error {
	if sc.Count() != c.Entries {
		return fmt.Errorf("%w: %d entries, recorded %d", ErrChunkMismatch, sc.Count(), c.Entries)
	}
	if c.Entries == 0 {
		return nil
	}
	first, last := sc.First(), sc.Last()
	firstTS, lastTS := pitr.Timestamp{T: first.TS.T, I: first.TS.I}, pitr.Timestamp{T: last.TS.T, I: last.TS.I}
	switch {
	case lastTS != c.To:
		return fmt.Errorf("%w: the last entry is at %s, the chunk ends at %s", ErrChunkMismatch, lastTS, c.To)
	case firstTS.Compare(c.From) < 0:
		return fmt.Errorf("%w: the first entry %s is before the chunk's start %s", ErrChunkMismatch, firstTS, c.From)
	case first.T != c.FirstTerm || last.T != c.LastTerm:
		return fmt.Errorf("%w: terms %d-%d, recorded %d-%d", ErrChunkMismatch, first.T, last.T, c.FirstTerm, c.LastTerm)
	}
	return nil
}

// decryptor returns the configured decryptor, if any.
func (s *Service) decryptor() interface {
	Decrypt(io.Reader) (io.Reader, error)
} {
	if s.cfg.Decryptor == nil {
		return nil
	}
	if d := s.cfg.Decryptor(); d != nil {
		return d
	}
	return nil
}

// VerifyChunks is the chunk item of the integrity sweep: it checks that the live
// chunks of every chain follow each other (each starts where the previous one
// ends) and verifies the chunks not verified within ReverifyAfter (VerifyChunk),
// recording each outcome. A failure raises verification.failed.
func (s *Service) VerifyChunks(ctx context.Context) (ChunkSweep, error) {
	var out ChunkSweep
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		return out, err
	}
	now := s.now()
	for _, st := range streams {
		chains, err := s.cfg.Repo.ListChains(ctx, st.ID)
		if err != nil {
			return out, err
		}
		for _, c := range chains {
			chunks, err := s.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: st.ID, ChainID: c.ChainID, Status: pitr.ChunkCommitted, Live: true})
			if err != nil {
				return out, err
			}
			for i := 1; i < len(chunks); i++ {
				if chunks[i].From != chunks[i-1].To {
					out.Breaks++
					msg := fmt.Sprintf("the chunk starts at %s but the previous one ends at %s", chunks[i].From, chunks[i-1].To)
					s.recordVerification(ctx, st, chunks[i], now, msg)
				}
			}
		}
	}
	due, err := s.cfg.Repo.ListChunksToVerify(ctx, now.Add(-ReverifyAfter), maxSweepChunks)
	if err != nil {
		return out, err
	}
	byID := map[string]*pitr.Stream{}
	for _, st := range streams {
		byID[st.ID] = st
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		st := byID[c.StreamID]
		if st == nil {
			continue
		}
		out.Verified++
		msg := ""
		if err := s.VerifyChunk(ctx, c); err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.Failed++
			msg = redact.Text(err.Error())
		}
		s.recordVerification(ctx, st, c, s.now(), msg)
	}
	return out, nil
}

// recordVerification stores the outcome of a chunk check and raises
// verification.failed for a failure.
func (s *Service) recordVerification(ctx context.Context, st *pitr.Stream, c *pitr.Chunk, at time.Time, msg string) {
	if err := s.cfg.Repo.MarkChunkVerified(ctx, c.ID, at, msg); err != nil && !errors.Is(err, pitr.ErrNotFound) {
		s.logger.Warn("cannot record a chunk verification", logsafe.Attr("chunk_id", c.ID), logsafe.Error(err))
	}
	if msg == "" {
		return
	}
	s.logger.Error("oplog chunk verification failed", logsafe.Attr("stream_id", st.ID), logsafe.Attr("chunk_id", c.ID), logsafe.Attr("error", msg))
	s.publish(ctx, events.Event{Type: events.VerificationFailed, Time: at, Status: "failed", Verification: "mismatch",
		Source: events.VerificationSweep, Error: msg, Detail: "oplog chunk " + c.ID, Stream: st.ID, ConnectionID: st.ConnectionID})
}
