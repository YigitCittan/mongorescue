package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// policy is a mutable API key policy for tests.
type policy struct {
	mu sync.Mutex
	p  auth.APIKeyPolicy
}

func (p *policy) get() auth.APIKeyPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.p
}

func (p *policy) set(v auth.APIKeyPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.p = v
}

func at(t time.Time) *time.Time { return &t }

func TestAnExpiredKeyIsRefusedFromItsExpiryOn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	exp := f.clock.Now().Add(48 * time.Hour)
	k, plain, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "ci", Scope: auth.ScopeRead, ExpiresAt: at(exp)})
	if err != nil {
		t.Fatal(err)
	}
	if k.ExpiresAt == nil || !k.ExpiresAt.Equal(exp) {
		t.Fatalf("ExpiresAt = %v; want %v", k.ExpiresAt, exp)
	}
	f.clock.Advance(48*time.Hour - time.Nanosecond)
	p, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatalf("a nanosecond before the expiry: %v", err)
	}
	if p.KeyExpiresAt == nil || !p.KeyExpiresAt.Equal(exp) {
		t.Errorf("KeyExpiresAt = %v", p.KeyExpiresAt)
	}
	f.clock.Advance(time.Nanosecond)
	_, err = f.svc.AuthenticateAPIKey(ctx, plain)
	var expired *auth.ExpiredKeyError
	if !errors.Is(err, auth.ErrAPIKeyExpired) || !errors.Is(err, auth.ErrUnauthenticated) || !errors.As(err, &expired) {
		t.Fatalf("at the expiry: %v; want an expired key", err)
	}
	if expired.KeyID != k.ID || expired.KeyName != "ci" || !expired.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiredKeyError = %+v", expired)
	}
	// A wrong secret with the prefix of the expired key is unknown, not expired.
	wrong := plain[:len(plain)-1] + "a"
	if plain[len(plain)-1] == 'a' {
		wrong = plain[:len(plain)-1] + "b"
	}
	if _, err = f.svc.AuthenticateAPIKey(ctx, wrong); !errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrAPIKeyExpired) {
		t.Errorf("a wrong secret: %v; want unauthenticated, not expired", err)
	}
}

func TestKeysWithoutAnExpiryStayValid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	k, plain, err := f.svc.CreateAPIKey(ctx, admin, "forever", auth.ScopeRead)
	if err != nil || k.ExpiresAt != nil {
		t.Fatalf("CreateAPIKey = %+v, %v", k, err)
	}
	f.clock.Advance(5 * 365 * 24 * time.Hour)
	if _, err := f.svc.AuthenticateAPIKey(ctx, plain); err != nil {
		t.Fatalf("a key without an expiry after five years: %v", err)
	}
}

func TestTheMaximumLifetimeCapsNewKeys(t *testing.T) {
	pol := &policy{}
	f := newFixture(t, auth.WithAPIKeyPolicy(pol.get))
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	now := f.clock.Now()
	create := func(exp *time.Time) error {
		_, _, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "k", ExpiresAt: exp})
		return err
	}
	if err := create(at(now)); !errors.Is(err, auth.ErrInvalidExpiry) {
		t.Errorf("an expiry of now: %v", err)
	}
	if err := create(at(now.Add(auth.MaxAPIKeyLifetime + time.Second))); !errors.Is(err, auth.ErrInvalidExpiry) {
		t.Errorf("beyond the absolute maximum: %v", err)
	}
	pol.set(auth.APIKeyPolicy{MaxLifetime: 30 * 24 * time.Hour})
	if err := create(nil); !errors.Is(err, auth.ErrInvalidExpiry) {
		t.Errorf("no expiry with a maximum lifetime: %v", err)
	}
	if err := create(at(now.Add(30*24*time.Hour + time.Second))); !errors.Is(err, auth.ErrInvalidExpiry) {
		t.Errorf("beyond the maximum lifetime: %v", err)
	}
	if err := create(at(now.Add(30 * 24 * time.Hour))); err != nil {
		t.Errorf("exactly the maximum lifetime: %v", err)
	}
}

func TestAMaximumLifetimeGivesExistingKeysADeadline(t *testing.T) {
	pol := &policy{}
	f := newFixture(t, auth.WithAPIKeyPolicy(pol.get))
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	old, oldPlain, err := f.svc.CreateAPIKey(ctx, admin, "old", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	soon, _, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "soon", ExpiresAt: at(f.clock.Now().Add(24 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	late, _, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "late", ExpiresAt: at(f.clock.Now().Add(400 * 24 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.ImportAPIKey(ctx, "an-imported-key-of-some-length"); err != nil {
		t.Fatal(err)
	}

	// Without a maximum lifetime nothing changes; soon is announced once.
	c, err := f.svc.CheckAPIKeyExpiry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Capped) != 0 || len(c.Announce) != 1 || c.Announce[0].ID != soon.ID || c.ExpiringCount != 1 || len(c.NeverExpiring) != 0 {
		t.Fatalf("check without a maximum = %+v", c)
	}
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil || len(c.Announce) != 0 {
		t.Fatalf("second check announced again: %+v, %v", c, err)
	}

	pol.set(auth.APIKeyPolicy{MaxLifetime: 90 * 24 * time.Hour, WarnBefore: 7 * 24 * time.Hour})
	deadline := f.clock.Now().Add(90 * 24 * time.Hour)
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, k := range c.Capped {
		ids[k.ID] = true
		if !k.ExpiresAt.Equal(deadline) {
			t.Errorf("capped %s expires at %v; want %v", k.Name, k.ExpiresAt, deadline)
		}
	}
	if len(ids) != 2 || !ids[old.ID] || !ids[late.ID] || !c.Deadline.Equal(deadline) {
		t.Fatalf("capped %v; want old and late (not soon, not the imported key)", ids)
	}
	if len(c.NeverExpiring) != 1 || c.NeverExpiring[0].CreatedBy != "" {
		t.Errorf("NeverExpiring = %+v; want the imported key", c.NeverExpiring)
	}
	// The deadline holds on later checks, and the old key still works until then.
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil || len(c.Capped) != 0 {
		t.Fatalf("a second check capped again: %+v, %v", c, err)
	}
	if _, err = f.svc.AuthenticateAPIKey(ctx, oldPlain); err != nil {
		t.Fatalf("old key before the deadline: %v", err)
	}
	f.clock.Advance(84 * 24 * time.Hour)
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.Announce) != 2 || c.ExpiringCount != 2 || c.ExpiredCount != 1 {
		t.Errorf("near the deadline: announce %d, expiring %d, expired %d; want 2, 2, 1", len(c.Announce), c.ExpiringCount, c.ExpiredCount)
	}
	f.clock.Advance(6 * 24 * time.Hour)
	if _, err = f.svc.AuthenticateAPIKey(ctx, oldPlain); !errors.Is(err, auth.ErrAPIKeyExpired) {
		t.Fatalf("old key at the deadline: %v", err)
	}
}

func TestExpiryStatusShowsOnlyTheCallersKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	viewer := f.signIn(t, admin, "vera", auth.RoleViewer)
	soon := at(f.clock.Now().Add(time.Hour))
	if _, _, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "admin-key", ExpiresAt: soon}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.CreateAPIKeyWithOptions(ctx, viewer, auth.APIKeyOptions{Name: "vera-key", ExpiresAt: soon}); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.APIKeyExpiryStatus(ctx, viewer)
	if err != nil || len(st.Expiring) != 1 || st.Expiring[0].Name != "vera-key" {
		t.Fatalf("viewer status = %+v, %v", st, err)
	}
	if st, err = f.svc.APIKeyExpiryStatus(ctx, admin); err != nil || len(st.Expiring) != 2 {
		t.Fatalf("admin status = %+v, %v", st, err)
	}
}

func TestKeysMintedByTheImportedKeyAreCapped(t *testing.T) {
	pol := &policy{}
	f := newFixture(t, auth.WithAPIKeyPolicy(pol.get))
	ctx := context.Background()
	f.setup(t)
	const imported = "an-imported-key-of-some-length"
	if _, err := f.svc.ImportAPIKey(ctx, imported); err != nil {
		t.Fatal(err)
	}
	importer, err := f.svc.AuthenticateAPIKey(ctx, imported)
	if err != nil {
		t.Fatal(err)
	}
	minted, _, err := f.svc.CreateAPIKey(ctx, importer, "minted", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if minted.CreatedBy != "" || minted.CreatedByKey != importer.APIKeyID || minted.Imported {
		t.Fatalf("minted = %+v; want no user, created_by_key %s, not imported", minted, importer.APIKeyID)
	}
	pol.set(auth.APIKeyPolicy{MaxLifetime: 30 * 24 * time.Hour})
	c, err := f.svc.CheckAPIKeyExpiry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Capped) != 1 || c.Capped[0].ID != minted.ID {
		t.Fatalf("capped %+v; want only the minted key", c.Capped)
	}
	if len(c.NeverExpiring) != 1 || c.NeverExpiring[0].ID != importer.APIKeyID || !c.NeverExpiring[0].Imported {
		t.Fatalf("never expiring %+v; want only the imported key", c.NeverExpiring)
	}
	// The imported key may not mint a key without an expiry under the maximum either.
	if _, _, err = f.svc.CreateAPIKey(ctx, importer, "forever", auth.ScopeRead); !errors.Is(err, auth.ErrInvalidExpiry) {
		t.Fatalf("minting without an expiry: %v", err)
	}
}

func TestAClockSteppingBackDoesNotCapAgain(t *testing.T) {
	pol := &policy{}
	f := newFixture(t, auth.WithAPIKeyPolicy(pol.get))
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	if _, _, err := f.svc.CreateAPIKey(ctx, admin, "old", auth.ScopeRead); err != nil {
		t.Fatal(err)
	}
	pol.set(auth.APIKeyPolicy{MaxLifetime: 30 * 24 * time.Hour})
	// Created under the maximum: checked against it already.
	if _, _, err := f.svc.CreateAPIKeyWithOptions(ctx, admin, auth.APIKeyOptions{Name: "new", ExpiresAt: at(f.clock.Now().Add(30 * 24 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	c, err := f.svc.CheckAPIKeyExpiry(ctx)
	if err != nil || len(c.Capped) != 1 || c.Capped[0].Name != "old" {
		t.Fatalf("first check = %+v, %v; want old capped", c, err)
	}
	f.clock.Advance(-2 * time.Hour)
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil || len(c.Capped) != 0 || len(c.Announce) != 0 {
		t.Fatalf("after the clock stepped back: %+v, %v; want nothing capped or announced", c, err)
	}
	// A lower maximum is a new one: it caps again.
	pol.set(auth.APIKeyPolicy{MaxLifetime: 10 * 24 * time.Hour})
	if c, err = f.svc.CheckAPIKeyExpiry(ctx); err != nil || len(c.Capped) != 2 {
		t.Fatalf("a lower maximum capped %d keys, %v; want 2", len(c.Capped), err)
	}
}
