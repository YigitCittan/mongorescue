package ha

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// swapPurpose derives the key of the swap marker MAC from secret.key.
const swapPurpose = "ha-swap-marker"

// swapMACContext prefixes the authenticated bytes of a swap marker.
const swapMACContext = "mongorescue-ha-swap/1"

// maxFinishAttempts bounds the compare-and-swap retries of SwapBoard.Finish.
const maxFinishAttempts = 4

// swapIDPattern is the shape of a restore ID in a marker key: it keeps a marker
// inside the swaps prefix whatever a record says.
var swapIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// SwapsPrefix returns the key prefix of the swap markers of cluster.
func SwapsPrefix(cluster string) string { return Prefix + cluster + "/swaps/" }

// SwapKey returns the storage key of the swap marker of restore restoreID.
func SwapKey(cluster, restoreID string) string { return SwapsPrefix(cluster) + restoreID + ".json" }

// SwapMACKey derives the key that authenticates swap markers from secret.key.
func SwapMACKey(secretKey []byte) ([]byte, error) {
	key, err := secretbox.DeriveSubkey(secretKey, swapPurpose)
	if err != nil {
		return nil, fmt.Errorf("ha: derive the swap marker key: %w", err)
	}
	return key, nil
}

// swapEnvelope is the content of a marker object: the marker as JSON and its MAC
// (HMAC-SHA256, hex) over the cluster and those exact bytes.
type swapEnvelope struct {
	Marker json.RawMessage `json:"marker"`
	MAC    string          `json:"mac"`
}

// swapMAC returns the MAC of raw, the JSON of a marker of cluster, under key.
func swapMAC(cluster string, raw, key []byte) string {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(swapMACContext + "\x00" + cluster + "\x00"))
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// SwapBoard is the swap ledger of an HA cluster (models.SwapLedger): one small
// authenticated object per in-flight in-place point-in-time swap on the HA
// target. Safety rests on three rules (docs/design/ha.md):
//
//  1. Begin writes the marker and only then checks the fence, so a swap that
//     replaces a database always has a marker that every later takeover, which
//     happens after the lease deadline the check passed, can list.
//  2. The marker is written under the swapping instance's epoch and
//     authenticated with a key derived from secret.key, so an object nobody holding
//     the key wrote never ends a swap.
//  3. Only the swapping process finishes a marker; nobody else ever claims a swap
//     stopped writing.
type SwapBoard struct {
	cluster string
	leases  storage.LeaseStore
	objects storage.Storage
	key     []byte
	fence   models.Fence
	logger  *slog.Logger
	now     func() time.Time
}

// NewSwapBoard returns the swap ledger of cluster on the HA target st (which must
// support conditional writes), authenticated with the key of SwapMACKey and
// fenced by fence. It fails when the cluster name or the target is unusable.
func NewSwapBoard(cluster string, st storage.Storage, secretKey []byte, fence models.Fence, logger *slog.Logger) (*SwapBoard, error) {
	if err := ValidateCluster(cluster); err != nil {
		return nil, err
	}
	ls, err := storage.AsLeaseStore(st)
	if err != nil {
		return nil, fmt.Errorf("ha: swap ledger: %w", err)
	}
	key, err := SwapMACKey(secretKey)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SwapBoard{cluster: cluster, leases: ls, objects: st, key: key, fence: fence, logger: logger, now: time.Now}, nil
}

// encode returns the object content of m.
func (b *SwapBoard) encode(m models.SwapMarker) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("ha: encode swap marker: %w", err)
	}
	data, err := json.Marshal(swapEnvelope{Marker: raw, MAC: swapMAC(b.cluster, raw, b.key)})
	if err != nil {
		return nil, fmt.Errorf("ha: encode swap marker: %w", err)
	}
	return data, nil
}

// errUnauthenticSwap reports a marker whose MAC or key does not check.
var errUnauthenticSwap = errors.New("ha: swap marker is not authentic")

// decode verifies and parses the content of the marker object of restoreID.
func (b *SwapBoard) decode(data []byte, restoreID string) (models.SwapMarker, error) {
	var env swapEnvelope
	if err := json.Unmarshal(data, &env); err != nil || len(env.Marker) == 0 {
		return models.SwapMarker{}, fmt.Errorf("%w: unreadable", errUnauthenticSwap)
	}
	mac, err := hex.DecodeString(env.MAC)
	want, _ := hex.DecodeString(swapMAC(b.cluster, env.Marker, b.key))
	if err != nil || !hmac.Equal(mac, want) {
		return models.SwapMarker{}, fmt.Errorf("%w: wrong MAC", errUnauthenticSwap)
	}
	var m models.SwapMarker
	if err := json.Unmarshal(env.Marker, &m); err != nil {
		return models.SwapMarker{}, fmt.Errorf("%w: %w", errUnauthenticSwap, err)
	}
	if m.RestoreID != restoreID {
		return models.SwapMarker{}, fmt.Errorf("%w: it belongs to restore %q, not %q", errUnauthenticSwap, m.RestoreID, restoreID)
	}
	return m, nil
}

// Begin implements models.SwapLedger.
func (b *SwapBoard) Begin(ctx context.Context, m models.SwapMarker) error {
	if !swapIDPattern.MatchString(m.RestoreID) {
		return fmt.Errorf("%w: invalid restore ID", models.ErrSwapLedger)
	}
	m.Phase, m.Outcome, m.Swapped, m.FinishedAt = models.SwapPhaseSwapping, "", nil, time.Time{}
	if m.StartedAt.IsZero() {
		m.StartedAt = b.now().UTC()
	}
	data, err := b.encode(m)
	if err != nil {
		return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
	}
	if _, err = b.leases.CreateIfAbsent(ctx, SwapKey(b.cluster, m.RestoreID), data); err != nil {
		if errors.Is(err, storage.ErrPreconditionFailed) {
			return fmt.Errorf("%w: %s", models.ErrSwapMarkerExists, m.RestoreID)
		}
		return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
	}
	// The marker is committed. If the lease is still held now, it was committed
	// before the deadline after which any takeover can happen, so every
	// takeover's listing sees it.
	if err = b.fence.Check(); err != nil {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if finErr := b.Finish(finishCtx, m.RestoreID, models.SwapAborted, nil); finErr != nil {
			b.logger.WarnContext(ctx, "could not mark a swap marker aborted; the next active instance times it out",
				logsafe.Attr("restore_id", m.RestoreID), logsafe.Error(finErr))
		}
		return err
	}
	return nil
}

// Finish implements models.SwapLedger.
func (b *SwapBoard) Finish(ctx context.Context, restoreID string, outcome models.SwapOutcome, swapped []string) error {
	if !swapIDPattern.MatchString(restoreID) {
		return fmt.Errorf("%w: invalid restore ID", models.ErrSwapLedger)
	}
	key := SwapKey(b.cluster, restoreID)
	var lastErr error
	for range maxFinishAttempts {
		data, version, err := b.leases.ReadVersioned(ctx, key)
		if err != nil {
			return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
		}
		m, err := b.decode(data, restoreID)
		if err != nil {
			return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
		}
		if m.Finished() {
			return nil
		}
		m.Phase, m.Outcome, m.Swapped, m.FinishedAt = models.SwapPhaseFinished, outcome, append([]string(nil), swapped...), b.now().UTC()
		out, err := b.encode(m)
		if err != nil {
			return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
		}
		_, err = b.leases.ReplaceIfMatch(ctx, key, out, version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrPreconditionFailed) {
			return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
		}
		lastErr = err
	}
	return fmt.Errorf("%w: %w", models.ErrSwapLedger, lastErr)
}

// Open implements models.SwapLedger.
func (b *SwapBoard) Open(ctx context.Context) ([]models.SwapMarker, error) {
	prefix := SwapsPrefix(b.cluster)
	objs, err := b.objects.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("%w: list: %w", models.ErrSwapLedger, err)
	}
	var out []models.SwapMarker
	for _, o := range objs {
		if o == nil || !strings.HasPrefix(o.Key, prefix) || !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(o.Key, prefix), ".json")
		if !swapIDPattern.MatchString(id) {
			continue
		}
		data, _, readErr := b.leases.ReadVersioned(ctx, o.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("%w: read %s: %w", models.ErrSwapLedger, o.Key, readErr)
		}
		m, decErr := b.decode(data, id)
		if decErr != nil {
			b.logger.ErrorContext(ctx, "an object in the swap ledger of the HA target is not authentic and is ignored",
				logsafe.Attr("key", o.Key), logsafe.Error(decErr))
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Resolve implements models.SwapLedger.
func (b *SwapBoard) Resolve(ctx context.Context, restoreID string) error {
	if !swapIDPattern.MatchString(restoreID) {
		return fmt.Errorf("%w: invalid restore ID", models.ErrSwapLedger)
	}
	if err := b.objects.Delete(ctx, SwapKey(b.cluster, restoreID)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("%w: %w", models.ErrSwapLedger, err)
	}
	return nil
}
