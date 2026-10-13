package ha

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

// The authenticated header of a metadata snapshot (docs/design/ha.md#metadata-snapshots).
// The plaintext of a snapshot is snapshotMagic, the header as one line of JSON, then
// the database. age authenticates the whole plaintext, so only a holder of
// secret.key can write a header: the cluster, epoch and time it names are those of
// the instance that wrote the snapshot, whatever the object key says.
const (
	// SnapshotFormat is the version of the snapshot header writers write.
	SnapshotFormat = 1
	// snapshotMagic starts the plaintext of a snapshot with a header. A legacy
	// snapshot (no header) starts with the SQLite file header instead.
	snapshotMagic = "mongorescue-ha-snapshot\n"
	// maxSnapshotHeader bounds the JSON line of a header.
	maxSnapshotHeader = 4096
	// MaxSnapshotsChecked bounds how many snapshot headers one decision (takeover,
	// epoch floor, retention, startup check) reads; snapshots beyond it are neither
	// restored nor deleted.
	MaxSnapshotsChecked = 64
)

// Errors of the snapshot header.
var (
	// ErrSnapshotRejected reports a snapshot whose authenticated header does not
	// match its object key or the cluster: a genuine snapshot copied to another
	// name, epoch or cluster. It is never restored nor deleted.
	ErrSnapshotRejected = errors.New("ha: the metadata snapshot's authenticated header does not match its name or cluster; it was copied or renamed and is refused")
	// ErrSnapshotLegacy reports a snapshot without an authenticated header, written
	// by a version before the header existed. It is restored only while the cluster
	// has no snapshot with a header.
	ErrSnapshotLegacy = errors.New("ha: the metadata snapshot has no authenticated header (written by an older version)")
	// ErrSnapshotFormat reports a snapshot header of a format this version does not
	// know (written by a newer version).
	ErrSnapshotFormat = errors.New("ha: the metadata snapshot was written in a newer format; upgrade this instance")
)

// SnapshotHeader is the authenticated header of a snapshot.
type SnapshotHeader struct {
	// Format is the header version (SnapshotFormat).
	Format int `json:"format"`
	// Cluster is the cluster the snapshot belongs to.
	Cluster string `json:"cluster"`
	// Epoch is the lease epoch it was written under.
	Epoch uint64 `json:"epoch"`
	// InstanceID is the instance that wrote it.
	InstanceID string `json:"instance_id"`
	// CreatedAt is when it was taken, to the millisecond (as in its key).
	CreatedAt time.Time `json:"created_at"`
}

// encode returns the plaintext prefix of h: the magic and one line of JSON.
func (h SnapshotHeader) encode() ([]byte, error) {
	data, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("ha: encode the snapshot header: %w", err)
	}
	if len(data) > maxSnapshotHeader {
		return nil, fmt.Errorf("ha: the snapshot header is %d bytes, more than %d", len(data), maxSnapshotHeader)
	}
	out := make([]byte, 0, len(snapshotMagic)+len(data)+1)
	out = append(out, snapshotMagic...)
	out = append(out, data...)
	return append(out, '\n'), nil
}

// parseSnapshotHeader reads the header from the start of a snapshot's plaintext,
// leaving r at the first byte of the database. It returns ErrSnapshotLegacy (r
// untouched) when the plaintext has no header. r must buffer more than
// maxSnapshotHeader bytes.
func parseSnapshotHeader(r *bufio.Reader) (*SnapshotHeader, error) {
	start, err := r.Peek(len(snapshotMagic))
	if !bytes.Equal(start, []byte(snapshotMagic)) {
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return nil, ErrSnapshotLegacy
	}
	if _, err = r.Discard(len(snapshotMagic)); err != nil {
		return nil, err
	}
	line, err := r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull) || len(line) > maxSnapshotHeader+1:
		return nil, fmt.Errorf("%w: the header is longer than %d bytes", ErrSnapshotRejected, maxSnapshotHeader)
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%w: truncated header", ErrSnapshotRejected)
	case err != nil:
		return nil, err
	}
	var h SnapshotHeader
	if err = json.Unmarshal(line, &h); err != nil {
		return nil, fmt.Errorf("%w: malformed header: %w", ErrSnapshotRejected, err)
	}
	if h.Format != SnapshotFormat {
		if h.Format > SnapshotFormat {
			return nil, fmt.Errorf("%w (format %d)", ErrSnapshotFormat, h.Format)
		}
		return nil, fmt.Errorf("%w: unknown header format %d", ErrSnapshotRejected, h.Format)
	}
	return &h, nil
}

// verify checks that h belongs to cluster and to the snapshot stored under key:
// the cluster, epoch and time of the key are those h authenticates.
func (h *SnapshotHeader) verify(cluster, key string) error {
	if !strings.HasPrefix(key, SnapshotPrefix(cluster)) {
		return fmt.Errorf("%w: %s is not a snapshot of cluster %q", ErrSnapshotRejected, key, cluster)
	}
	at, epoch, ok := parseSnapshotKey(key)
	switch {
	case !ok:
		return fmt.Errorf("%w: %s is not a snapshot name", ErrSnapshotRejected, key)
	case h.Cluster != cluster:
		return fmt.Errorf("%w: %s was written for cluster %q, not %q", ErrSnapshotRejected, key, h.Cluster, cluster)
	case h.Epoch != epoch:
		return fmt.Errorf("%w: %s was written under epoch %d, its name says %d", ErrSnapshotRejected, key, h.Epoch, epoch)
	case !h.CreatedAt.Equal(at):
		return fmt.Errorf("%w: %s was taken at %s, its name says %s", ErrSnapshotRejected, key,
			h.CreatedAt.UTC().Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
	return nil
}

// openSnapshot downloads snapshot key, decrypts it and reads and verifies its
// header. The header is nil for a legacy snapshot. On error, rc is nil and err
// wraps ErrSnapshotUnreadable, ErrSnapshotRejected (h is then the authentic but
// mismatching header, or nil when it is malformed), ErrSnapshotFormat or a storage
// error. Otherwise the caller reads the database from plain and closes rc.
func openSnapshot(ctx context.Context, st storage.Storage, cluster, key, passphrase string) (h *SnapshotHeader, plain *bufio.Reader, rc io.Closer, err error) {
	dec, err := snapshotDecryptor(passphrase)
	if err != nil {
		return nil, nil, nil, err
	}
	obj, err := st.Retrieve(ctx, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ha: download snapshot %s: %w", key, err)
	}
	defer func() {
		if err != nil {
			_ = obj.Close()
		}
	}()
	r, err := dec.Decrypt(obj)
	if err != nil {
		return nil, nil, nil, snapshotDecryptError(key, err)
	}
	plain = bufio.NewReaderSize(&ctxReader{ctx: ctx, r: r}, 2*maxSnapshotHeader)
	h, err = parseSnapshotHeader(plain)
	switch {
	case errors.Is(err, ErrSnapshotLegacy):
		return nil, plain, obj, nil
	case errors.Is(err, ErrSnapshotRejected) || errors.Is(err, ErrSnapshotFormat):
		return nil, nil, nil, fmt.Errorf("snapshot %s: %w", key, err)
	case err != nil:
		return nil, nil, nil, snapshotDecryptError(key, err)
	}
	if err = h.verify(cluster, key); err != nil {
		return h, nil, nil, err
	}
	return h, plain, obj, nil
}

// SnapshotCheck is the outcome of reading the header of a snapshot.
type SnapshotCheck struct {
	SnapshotInfo
	// Header is the authenticated header: set for a verified snapshot, and for one
	// refused because the header does not match its key (whose authentic epoch it
	// still tells); nil otherwise.
	Header *SnapshotHeader
	// Err is nil for a verified snapshot. Otherwise it wraps ErrSnapshotLegacy,
	// ErrSnapshotRejected, ErrSnapshotUnreadable (it does not decrypt: tampered, or
	// another secret key), ErrSnapshotFormat or a storage error.
	Err error
}

// Verified reports whether the snapshot's header matched its key and cluster.
func (c SnapshotCheck) Verified() bool { return c.Err == nil && c.Header != nil }

// Legacy reports whether the snapshot has no header.
func (c SnapshotCheck) Legacy() bool { return errors.Is(c.Err, ErrSnapshotLegacy) }

// Refused reports whether the snapshot failed authentication: its header does not
// match its key or cluster, or it does not decrypt. Such snapshots are alerted.
func (c SnapshotCheck) Refused() bool {
	return errors.Is(c.Err, ErrSnapshotRejected) || errors.Is(c.Err, ErrSnapshotUnreadable)
}

// determinate reports whether the check settled what the snapshot is (a storage
// error did not).
func (c SnapshotCheck) determinate() bool {
	return c.Err == nil || c.Legacy() || c.Refused() || errors.Is(c.Err, ErrSnapshotFormat)
}

// CheckSnapshot reads and verifies the header of snapshot info of cluster,
// downloading only the start of the snapshot.
func CheckSnapshot(ctx context.Context, st storage.Storage, cluster, passphrase string, info SnapshotInfo) SnapshotCheck {
	h, _, rc, err := openSnapshot(ctx, st, cluster, info.Key, passphrase)
	if rc != nil {
		_ = rc.Close()
	}
	if err == nil && h == nil {
		err = fmt.Errorf("snapshot %s: %w", info.Key, ErrSnapshotLegacy)
	}
	return SnapshotCheck{SnapshotInfo: info, Header: h, Err: err}
}

// SnapshotSelection is what SelectSnapshots found on the HA target.
type SnapshotSelection struct {
	// Verified holds the verified snapshots found, highest epoch then newest: at
	// most the number asked for.
	Verified []SnapshotCheck
	// Legacy holds the snapshots without a header found, in the same order.
	Legacy []SnapshotCheck
	// Refused holds the snapshots that failed authentication (see
	// SnapshotCheck.Refused).
	Refused []SnapshotCheck
	// Failed holds the snapshots whose check failed otherwise (a storage error, a
	// newer format).
	Failed []SnapshotCheck
	// Listed is how many snapshots the HA target lists.
	Listed int
	// Complete reports that the header of every listed snapshot was read.
	Complete bool
	// headerSeen reports an authentic header of the cluster (verified, renamed or
	// of a newer format): the cluster writes headers, so legacy snapshots are stale.
	headerSeen bool
}

// Candidates returns the snapshots a takeover may restore, in order: the verified
// ones; or, only when every snapshot was checked and none of the cluster's has an
// authentic header, the legacy ones (legacy is then true).
func (s SnapshotSelection) Candidates() (list []SnapshotCheck, legacy bool) {
	if len(s.Verified) > 0 {
		return s.Verified, false
	}
	if s.Complete && !s.headerSeen && len(s.Legacy) > 0 {
		return s.Legacy, true
	}
	return nil, false
}

// AuthenticEpoch returns the highest epoch the selection proves was used: of the
// verified snapshots and of the authentic headers of the cluster's renamed ones;
// or, when a takeover would restore legacy snapshots, of their keys. ok is false
// when it is unknown: no snapshot is verified and not all could be checked.
func (s SnapshotSelection) AuthenticEpoch(cluster string) (epoch uint64, ok bool) {
	for _, c := range s.Verified {
		epoch = max(epoch, c.Header.Epoch)
	}
	for _, c := range s.Refused {
		if c.Header != nil && c.Header.Cluster == cluster {
			epoch = max(epoch, c.Header.Epoch)
		}
	}
	if list, legacy := s.Candidates(); legacy {
		for _, c := range list {
			epoch = max(epoch, c.Epoch)
		}
	}
	return epoch, len(s.Verified) > 0 || s.Complete
}

// SelectSnapshots reads the headers of the snapshots of cluster in the order of
// ListSnapshots until it has found want verified ones (at most
// MaxSnapshotsChecked headers). It fails only when the snapshots cannot be listed
// or ctx ends.
func SelectSnapshots(ctx context.Context, st storage.Storage, cluster, passphrase string, want int) (SnapshotSelection, error) {
	list, err := ListSnapshots(ctx, st, cluster)
	if err != nil {
		return SnapshotSelection{}, err
	}
	return selectSnapshots(ctx, cluster, list, want, func(info SnapshotInfo) SnapshotCheck {
		return CheckSnapshot(ctx, st, cluster, passphrase, info)
	})
}

// selectSnapshots classifies list (ListSnapshots order) of cluster with check,
// stopping after want verified snapshots or MaxSnapshotsChecked checks.
func selectSnapshots(ctx context.Context, cluster string, list []SnapshotInfo, want int, check func(SnapshotInfo) SnapshotCheck) (SnapshotSelection, error) {
	sel := SnapshotSelection{Listed: len(list), Complete: true}
	for i, info := range list {
		if len(sel.Verified) >= max(want, 1) || i >= MaxSnapshotsChecked {
			sel.Complete = false
			break
		}
		c := check(info)
		if err := ctx.Err(); err != nil {
			return SnapshotSelection{}, err
		}
		switch {
		case c.Verified():
			sel.Verified = append(sel.Verified, c)
		case c.Legacy():
			sel.Legacy = append(sel.Legacy, c)
		case c.Refused():
			sel.Refused = append(sel.Refused, c)
		default:
			sel.Failed = append(sel.Failed, c)
		}
		if (c.Header != nil && c.Header.Cluster == cluster) || errors.Is(c.Err, ErrSnapshotFormat) {
			// A renamed snapshot's header, or a newer one, is still authentic.
			sel.headerSeen = true
		}
		if !c.determinate() {
			sel.Complete = false
		}
	}
	return sel, nil
}

// FetchSnapshot downloads snapshot key of cluster from st, decrypts it, verifies
// its header against key and cluster, and writes the database to the new file path
// (mode 0600), streaming. It returns the header, nil for a legacy snapshot, which
// is restored only when allowLegacy is set (else an error wrapping
// ErrSnapshotLegacy). A snapshot whose header does not match is refused before the
// file is created (ErrSnapshotRejected); one that passphrase does not decrypt
// returns an error wrapping ErrSnapshotUnreadable.
func FetchSnapshot(ctx context.Context, st storage.Storage, cluster, key, passphrase, path string, allowLegacy bool) (h *SnapshotHeader, err error) {
	h, plain, rc, err := openSnapshot(ctx, st, cluster, key, passphrase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	if h == nil && !allowLegacy {
		return nil, fmt.Errorf("snapshot %s: %w; it is restored only while the cluster has no snapshot with a header", key, ErrSnapshotLegacy)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ha: create the snapshot file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("ha: write the snapshot file: %w", closeErr)
		}
		if err != nil {
			h = nil
			_ = os.Remove(path)
		}
	}()
	if _, err = io.Copy(f, plain); err != nil {
		return nil, snapshotDecryptError(key, err)
	}
	if err = f.Sync(); err != nil {
		return nil, fmt.Errorf("ha: write the snapshot file: %w", err)
	}
	return h, nil
}
