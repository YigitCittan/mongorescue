package vaultcreds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func kvRef(path string) models.VaultRef {
	r := models.VaultRef{Engine: models.VaultKV2, Path: path}
	_ = r.Normalize(models.VaultForConnection)
	return r
}

func dbRef() models.VaultRef {
	r := models.VaultRef{Engine: models.VaultDatabase, Role: "backup"}
	_ = r.Normalize(models.VaultForConnection)
	return r
}

func TestKV2ReadAndCache(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/mongo/prod"] = map[string]any{"username": "backup", "password": "kv-pass-1", "other": 3}
	r := newTestResolver(t, srv, time.Second, nil)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	up, err := r.Connection(ctx, kvRef("mongo/prod"))
	if err != nil || up.Username != "backup" || up.Password != "kv-pass-1" {
		t.Fatalf("Connection = %+v, %v", up, err)
	}
	// Within the cache TTL Vault is not read again.
	now = now.Add(DefaultCacheTTL - time.Second)
	f.set(func(f *fakeVault) { f.kv["secret/mongo/prod"]["password"] = "kv-pass-2" })
	if up, _ = r.Connection(ctx, kvRef("mongo/prod")); up.Password != "kv-pass-1" || get(f, func(f *fakeVault) int { return f.reads }) != 1 {
		t.Fatalf("cached read = %+v, reads %d", up, f.reads)
	}
	// Once it expired, the rotated value is read.
	now = now.Add(2 * time.Second)
	if up, _ = r.Connection(ctx, kvRef("mongo/prod")); up.Password != "kv-pass-2" || get(f, func(f *fakeVault) int { return f.reads }) != 2 {
		t.Fatalf("read after expiry = %+v, reads %d", up, f.reads)
	}
}

func TestKV2InvalidSecrets(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/missing-field"] = map[string]any{"username": "u"}
	f.kv["secret/not-string"] = map[string]any{"username": "u", "password": 42}
	f.kv["secret/deleted"] = nil
	r := newTestResolver(t, srv, time.Second, nil)
	ctx := context.Background()
	for path, want := range map[string]error{"missing-field": ErrInvalidSecret, "not-string": ErrInvalidSecret, "deleted": ErrInvalidSecret, "absent": ErrRefused} {
		if _, err := r.Connection(ctx, kvRef(path)); !errors.Is(err, want) {
			t.Errorf("%s = %v; want %v", path, err, want)
		}
	}
}

func TestUnavailableAlertsOncePerEpisode(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/x"] = map[string]any{"username": "u", "password": "never-logged-pass"}
	var alerts []string
	r := newTestResolver(t, srv, time.Second, &alerts)
	r.cacheTTL = time.Nanosecond
	ctx := context.Background()
	f.set(func(f *fakeVault) { f.down = true })
	for range 3 {
		if _, err := r.Connection(ctx, kvRef("x")); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("down = %v; want ErrUnavailable", err)
		}
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "unavailable") {
		t.Fatalf("alerts = %q; want one", alerts)
	}
	// Recovery ends the episode; the next failure alerts again.
	f.set(func(f *fakeVault) { f.down = false })
	if _, err := r.Connection(ctx, kvRef("x")); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeVault) { f.down = true })
	_, _ = r.Connection(ctx, kvRef("x"))
	if len(alerts) != 2 {
		t.Fatalf("alerts after a new episode = %d; want 2", len(alerts))
	}
}

func TestNotConfigured(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Connection(context.Background(), kvRef("x")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured = %v", err)
	}
	if _, _, _, err := r.Hold(context.Background(), dbRef()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured hold = %v", err)
	}
}

func TestDynamicLeaseRenewedWhileHeldAndRevokedAfter(t *testing.T) {
	f, srv := newFakeVault(t)
	// 30 "seconds" of 10ms: renewed once 100ms are left.
	r := newTestResolver(t, srv, 10*time.Millisecond, nil)
	ctx := context.Background()

	up, err := r.Connection(ctx, dbRef())
	if err != nil || up.Username != "v-user-1" {
		t.Fatalf("Connection = %+v, %v", up, err)
	}
	// Two runs share the fresh lease of the preparation.
	ctx1, up1, release1, err := r.Hold(ctx, dbRef())
	if err != nil || up1 != up {
		t.Fatalf("Hold 1 = %+v, %v", up1, err)
	}
	ctx2, up2, release2, err := r.Hold(ctx, dbRef())
	if err != nil || up2 != up {
		t.Fatalf("Hold 2 = %+v, %v", up2, err)
	}
	// The lease outlives its first TTL: it is renewed.
	time.Sleep(800 * time.Millisecond)
	if n := get(f, func(f *fakeVault) int { return f.renewals }); n < 2 {
		t.Fatalf("renewals = %d; want at least 2", n)
	}
	if ctx1.Err() != nil || ctx2.Err() != nil {
		t.Fatalf("held contexts ended: %v %v", context.Cause(ctx1), context.Cause(ctx2))
	}
	release1()
	release1() // idempotent
	if get(f, func(f *fakeVault) int { return len(f.revoked) }) != 0 {
		t.Fatal("revoked while a run still holds the lease")
	}
	// The last run's release revokes the lease: nothing resolved it since.
	release2()
	eventually(t, "revocation", func() bool { return get(f, func(f *fakeVault) int { return len(f.revoked) }) == 1 })
	if ctx2.Err() == nil {
		t.Fatal("the released context is still live")
	}
	if get(f, func(f *fakeVault) int { return f.issued }) != 1 {
		t.Fatalf("issued %d leases; want 1", f.issued)
	}
	// A new run gets a new lease.
	_, up3, release3, err := r.Hold(ctx, dbRef())
	if err != nil || up3.Username != "v-user-2" {
		t.Fatalf("Hold after revocation = %+v, %v", up3, err)
	}
	release3()
}

func TestLeaseResolvedDuringARunOutlivesIt(t *testing.T) {
	f, srv := newFakeVault(t)
	r := newTestResolver(t, srv, time.Second, nil)
	ctx := context.Background()
	_, up, release, err := r.Hold(ctx, dbRef())
	if err != nil {
		t.Fatal(err)
	}
	// Another run resolves the lease while the first one holds it.
	next, err := r.Connection(ctx, dbRef())
	if err != nil || next != up {
		t.Fatalf("Connection during the run = %+v, %v", next, err)
	}
	release()
	time.Sleep(50 * time.Millisecond)
	if n := get(f, func(f *fakeVault) int { return len(f.revoked) }); n != 0 {
		t.Fatalf("revoked %d lease(s) another run is about to use", n)
	}
	// That run holds and releases it: now it is revoked.
	_, up2, release2, err := r.Hold(ctx, dbRef())
	if err != nil || up2 != up {
		t.Fatalf("second hold = %+v, %v", up2, err)
	}
	release2()
	eventually(t, "revocation", func() bool { return get(f, func(f *fakeVault) int { return len(f.revoked) }) == 1 })
}

func TestRenewalFailureEndsTheRunBeforeExpiry(t *testing.T) {
	f, srv := newFakeVault(t)
	var alerts []string
	r := newTestResolver(t, srv, 20*time.Millisecond, &alerts)
	ctx := context.Background()
	held, _, release, err := r.Hold(ctx, dbRef())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.set(func(f *fakeVault) { f.failRenew = true })
	start := time.Now()
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not stopped")
	}
	// It ends within the lease (600ms), retrying until the margin.
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Errorf("ended after %s, past the lease", el)
	}
	cause := context.Cause(held)
	if !errors.Is(cause, ErrLeaseLost) || !strings.Contains(cause.Error(), "renewal") {
		t.Fatalf("cause = %v", cause)
	}
	if get(f, func(f *fakeVault) int { return f.renewals }) < 2 {
		t.Errorf("renewal was not retried")
	}
	if len(alerts) == 0 {
		t.Error("no alert for the failed renewal")
	}
}

func TestTransientRenewalFailureKeepsTheRun(t *testing.T) {
	f, srv := newFakeVault(t)
	r := newTestResolver(t, srv, 10*time.Millisecond, nil)
	held, _, release, err := r.Hold(context.Background(), dbRef())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.set(func(f *fakeVault) { f.failRenew = true })
	eventually(t, "a failed renewal", func() bool { return get(f, func(f *fakeVault) int { return f.renewals }) >= 1 })
	f.set(func(f *fakeVault) { f.failRenew = false })
	time.Sleep(500 * time.Millisecond)
	if held.Err() != nil {
		t.Fatalf("the run ended although renewal recovered: %v", context.Cause(held))
	}
}

func TestMaxTTLEndsTheLeaseBeforeExpiry(t *testing.T) {
	f, srv := newFakeVault(t)
	f.maxTTL = 50
	r := newTestResolver(t, srv, 10*time.Millisecond, nil)
	held, _, release, err := r.Hold(context.Background(), dbRef())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lease did not end at its maximum TTL")
	}
	if cause := context.Cause(held); !errors.Is(cause, ErrLeaseLost) || !strings.Contains(cause.Error(), "maximum TTL") {
		t.Fatalf("cause = %v", cause)
	}
}

func TestNonRenewableLeaseEndsAtItsMargin(t *testing.T) {
	f, srv := newFakeVault(t)
	f.renewable = false
	r := newTestResolver(t, srv, 10*time.Millisecond, nil)
	held, _, release, err := r.Hold(context.Background(), dbRef())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lease did not end")
	}
	if !errors.Is(context.Cause(held), ErrLeaseLost) || get(f, func(f *fakeVault) int { return f.renewals }) != 0 {
		t.Fatalf("cause = %v, renewals %d", context.Cause(held), f.renewals)
	}
}

func TestStaleLeaseIsReplaced(t *testing.T) {
	f, srv := newFakeVault(t)
	r := newTestResolver(t, srv, 10*time.Millisecond, nil)
	ctx := context.Background()
	if _, err := r.Connection(ctx, dbRef()); err != nil {
		t.Fatal(err)
	}
	// Unheld, it is not renewed: once less than a quarter is left a new one is
	// issued and the old one revoked.
	time.Sleep(250 * time.Millisecond)
	up, err := r.Connection(ctx, dbRef())
	if err != nil || up.Username != "v-user-2" {
		t.Fatalf("Connection = %+v, %v", up, err)
	}
	eventually(t, "revocation of the stale lease", func() bool { return get(f, func(f *fakeVault) int { return len(f.revoked) }) == 1 })
}

func TestCloseRevokesAndEndsHolders(t *testing.T) {
	f, srv := newFakeVault(t)
	r := newTestResolver(t, srv, time.Second, nil)
	held, _, release, err := r.Hold(context.Background(), dbRef())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = r.S3(context.Background(), awsRef()); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if !errors.Is(context.Cause(held), ErrLeaseLost) {
		t.Fatalf("holder after Close = %v", context.Cause(held))
	}
	if n := get(f, func(f *fakeVault) int { return len(f.revoked) }); n != 2 {
		t.Fatalf("revoked %d leases at Close; want 2", n)
	}
}

func awsRef() models.VaultRef {
	r := models.VaultRef{Engine: models.VaultAWS, Role: "s3-backup"}
	_ = r.Normalize(models.VaultForS3)
	return r
}

func TestS3Keys(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/minio"] = map[string]any{"access_key": "minio-ak", "secret_key": "minio-sk"}
	r := newTestResolver(t, srv, time.Second, nil)
	ctx := context.Background()
	kv := models.VaultRef{Engine: models.VaultKV2, Path: "minio"}
	if err := kv.Normalize(models.VaultForS3); err != nil {
		t.Fatal(err)
	}
	creds, err := r.S3Provider(kv).Retrieve(ctx)
	if err != nil || creds.AccessKeyID != "minio-ak" || creds.SecretAccessKey != "minio-sk" || !creds.CanExpire {
		t.Fatalf("kv keys = %+v, %v", creds, err)
	}
	creds, err = r.S3Provider(awsRef()).Retrieve(ctx)
	if err != nil || creds.AccessKeyID != "AKIA1" || creds.SessionToken != "sts-token" || !creds.Expires.Before(time.Now().Add(30*time.Second)) {
		t.Fatalf("aws keys = %+v, %v", creds, err)
	}
	f.set(func(f *fakeVault) { f.down = true })
	if _, err = r.S3Provider(awsRef()).Retrieve(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("aws keys while Vault is down = %v", err)
	}
}

func TestTestNeverReturnsSecrets(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/mongo"] = map[string]any{"username": "kv-user-name", "password": "kv-secret-pass"}
	r := newTestResolver(t, srv, time.Second, nil)
	ctx := context.Background()
	res, err := r.Test(ctx, models.VaultForConnection, models.VaultRef{Engine: models.VaultKV2, Path: "mongo"})
	if err != nil || !res.OK || res.Version != 3 || len(res.Fields) != 2 {
		t.Fatalf("kv test = %+v, %v", res, err)
	}
	res2, err := r.Test(ctx, models.VaultForConnection, models.VaultRef{Engine: models.VaultDatabase, Role: "backup"})
	if err != nil || !res2.OK || res2.LeaseTTLSeconds != 30 || !res2.Renewable {
		t.Fatalf("database test = %+v, %v", res2, err)
	}
	// The test lease is revoked right away and nothing is cached.
	if get(f, func(f *fakeVault) int { return len(f.revoked) }) != 1 || len(r.leases) != 0 || len(r.static) != 0 {
		t.Fatalf("revoked %v, leases %d, static %d", f.revoked, len(r.leases), len(r.static))
	}
	for _, v := range []any{res, res2} {
		raw, _ := json.Marshal(v)
		for _, secret := range []string{"kv-secret-pass", "dyn-pass-1", "v-user-1", "kv-user-name"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("test result %s holds %q", raw, secret)
			}
		}
	}
	if _, err = r.Test(ctx, models.VaultForConnection, models.VaultRef{Engine: models.VaultAWS, Role: "x"}); !errors.Is(err, models.ErrInvalidVaultRef) {
		t.Fatalf("aws for a connection = %v", err)
	}
	f.kv["secret/half"] = map[string]any{"username": "u"}
	res3, _ := r.Test(ctx, models.VaultForConnection, models.VaultRef{Engine: models.VaultKV2, Path: "half"})
	if res3.OK || !strings.Contains(res3.Error, `"password"`) {
		t.Fatalf("missing field = %+v", res3)
	}
}

func TestErrorsNeverHoldValues(t *testing.T) {
	f, srv := newFakeVault(t)
	f.kv["secret/odd"] = map[string]any{"username": "kv-user-x", "password": []string{"array-secret"}}
	r := newTestResolver(t, srv, time.Second, nil)
	_, err := r.Connection(context.Background(), kvRef("odd"))
	if err == nil || strings.Contains(err.Error(), "array-secret") || strings.Contains(err.Error(), "kv-user-x") {
		t.Fatalf("error = %v", err)
	}
}
