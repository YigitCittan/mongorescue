package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

const (
	adminPassword = "correct horse battery"
	ip            = "192.0.2.10"
)

// clock is a manually advanced time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	svc   *auth.Service
	store *store.SQLiteStore
	clock *clock
}

func newFixture(t *testing.T, opts ...auth.Option) *fixture {
	t.Helper()
	f := &fixture{store: storetest.New(t), clock: &clock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}}
	opts = append([]auth.Option{auth.WithBcryptCost(bcrypt.MinCost), auth.WithClock(f.clock.Now)}, opts...)
	svc, err := auth.NewService(f.store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

// setup completes first-run setup and returns the admin session.
func (f *fixture) setup(t *testing.T) *auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	if required, err := f.svc.Init(ctx); err != nil || !required {
		t.Fatalf("Init = %v, %v; want setup required", required, err)
	}
	res, err := f.svc.Setup(ctx, ip, f.svc.SetupCode(), "admin", adminPassword)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return res
}

func (f *fixture) session(t *testing.T, token string) *auth.Principal {
	t.Helper()
	p, err := f.svc.AuthenticateSession(context.Background(), token)
	if err != nil {
		t.Fatalf("AuthenticateSession: %v", err)
	}
	return p
}

func TestSetupCodeIsSingleUseAndSetupRunsOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if required, err := f.svc.Init(ctx); err != nil || !required {
		t.Fatalf("Init on empty database = %v, %v", required, err)
	}
	code := f.svc.SetupCode()
	if len(strings.ReplaceAll(code, "-", "")) != 24 || strings.Count(code, "-") != 5 {
		t.Fatalf("setup code %q: want 24 base32 characters in 6 groups", code)
	}

	if _, err := f.svc.Setup(ctx, ip, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF", "admin", adminPassword); !errors.Is(err, auth.ErrInvalidSetupCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := f.svc.Setup(ctx, ip, code, "admin", "short"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if f.svc.SetupCode() != code {
		t.Fatal("a failed setup must not consume the code")
	}

	// Case and separators do not matter.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	res, err := f.svc.Setup(ctx, ip, typed, "admin", adminPassword)
	if err != nil || res.User.Username != "admin" || res.Token == "" || res.CSRFToken == "" {
		t.Fatalf("Setup = %+v, %v", res, err)
	}
	if f.svc.SetupCode() != "" {
		t.Fatal("the setup code must be cleared after use")
	}
	if _, err = f.svc.Setup(ctx, ip, code, "other", adminPassword); !errors.Is(err, auth.ErrSetupCompleted) {
		t.Fatalf("second setup with the same code: %v; want ErrSetupCompleted", err)
	}
	if required, _ := f.svc.SetupRequired(ctx); required {
		t.Fatal("setup must not be required once a user exists")
	}
	f.session(t, res.Token)

	// A restarted service with users issues no code.
	again, err := auth.NewService(f.store, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if required, err := again.Init(ctx); err != nil || required || again.SetupCode() != "" {
		t.Fatalf("Init with users = %v, %v, code %q", required, err, again.SetupCode())
	}
}

func TestSetupCodeIsNewPerRestart(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := auth.NewService(f.store, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.svc.SetupCode() == other.SetupCode() {
		t.Fatal("every start must generate a fresh setup code")
	}
}

func TestSetupWrongCodesNeverLockOutTheRightOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range auth.SetupFreeAttempts {
		if _, err := f.svc.Setup(ctx, ip, "wrong", "admin", adminPassword); !errors.Is(err, auth.ErrInvalidSetupCode) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	// Beyond the budget wrong codes are throttled ...
	_, err := f.svc.Setup(ctx, ip, "wrong", "admin", adminPassword)
	var throttled *auth.ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 {
		t.Fatalf("wrong code beyond the budget: %v; want throttled", err)
	}
	// ... other clients are unaffected ...
	if _, err := f.svc.Setup(ctx, "198.51.100.7", "wrong", "admin", adminPassword); !errors.Is(err, auth.ErrInvalidSetupCode) {
		t.Fatalf("other ip: %v", err)
	}
	// ... and the correct code from the same IP is still accepted.
	if _, err := f.svc.Setup(ctx, ip, f.svc.SetupCode(), "admin", adminPassword); err != nil {
		t.Fatalf("correct code after many wrong ones: %v", err)
	}
}

func TestSetupBudgetResetsAfterTheWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for range auth.SetupFreeAttempts + 1 {
		_, _ = f.svc.Setup(ctx, ip, "wrong", "admin", adminPassword)
	}
	f.clock.Advance(auth.SetupWindow)
	if _, err := f.svc.Setup(ctx, ip, "wrong", "admin", adminPassword); !errors.Is(err, auth.ErrInvalidSetupCode) {
		t.Fatalf("wrong code after the window: %v; want ErrInvalidSetupCode", err)
	}
}

func TestLoginGenericFailureAndThrottling(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()

	_, errUnknown := f.svc.Login(ctx, ip, "nobody", adminPassword)
	_, errWrong := f.svc.Login(ctx, ip, "admin", "wrong password!!")
	if !errors.Is(errUnknown, auth.ErrInvalidCredentials) || !errors.Is(errWrong, auth.ErrInvalidCredentials) ||
		errUnknown.Error() != errWrong.Error() {
		t.Fatalf("login failures must be generic: %v / %v", errUnknown, errWrong)
	}
	if res, err := f.svc.Login(ctx, ip, "ADMIN", adminPassword); err != nil || res.User.LastLoginAt == nil {
		t.Fatalf("login (case-insensitive username) = %+v, %v", res, err)
	}

	// Five more failures lock the pair out.
	for range auth.FreeAttempts {
		_, _ = f.svc.Login(ctx, ip, "admin", "wrong password!!")
	}
	_, err := f.svc.Login(ctx, ip, "admin", adminPassword)
	var throttled *auth.ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 || throttled.RetryAfter > auth.MaxLockout {
		t.Fatalf("login while locked out: %v", err)
	}
	// Another client IP is not affected.
	if _, err = f.svc.Login(ctx, "198.51.100.7", "admin", adminPassword); err != nil {
		t.Fatalf("login from another ip: %v", err)
	}

	// Each further failure doubles the lockout, capped at MaxLockout.
	first := throttled.RetryAfter
	f.clock.Advance(first)
	_, _ = f.svc.Login(ctx, ip, "admin", "wrong password!!")
	_, err = f.svc.Login(ctx, ip, "admin", adminPassword)
	if !errors.As(err, &throttled) || throttled.RetryAfter <= first {
		t.Fatalf("lockout must grow: %v (first %v)", err, first)
	}
	for range 20 {
		f.clock.Advance(auth.MaxLockout)
		_, _ = f.svc.Login(ctx, ip, "admin", "wrong password!!")
	}
	_, err = f.svc.Login(ctx, ip, "admin", adminPassword)
	if !errors.As(err, &throttled) || throttled.RetryAfter > auth.MaxLockout {
		t.Fatalf("lockout must be capped at %v: %v", auth.MaxLockout, err)
	}

	// After the lockout a correct password works and resets the counter.
	f.clock.Advance(auth.MaxLockout)
	if _, err := f.svc.Login(ctx, ip, "admin", adminPassword); err != nil {
		t.Fatalf("login after lockout: %v", err)
	}
	if _, err := f.svc.Login(ctx, ip, "admin", "wrong password!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("counter not reset after success: %v", err)
	}
}

func TestNoisyIPSlowsSprayingButNeverBlocksCorrectCredentials(t *testing.T) {
	f := newFixture(t)
	f.setup(t)
	ctx := context.Background()
	user := func(i int) string { return "user" + string(rune('a'+i%26)) + string(rune('a'+i/26)) }
	// Spray one wrong password per username until the IP counts as noisy.
	for i := range 4 * auth.FreeAttempts {
		if _, err := f.svc.Login(ctx, ip, user(i), "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("spray %d: %v", i, err)
		}
	}
	// Now every username from this IP gets a single attempt.
	fresh := user(4*auth.FreeAttempts + 1)
	if _, err := f.svc.Login(ctx, ip, fresh, "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("first attempt for a fresh username: %v", err)
	}
	if _, err := f.svc.Login(ctx, ip, fresh, "x"); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("second attempt from a noisy ip: %v; want throttled", err)
	}
	// The correct password of an unlocked user from the same (shared) IP still works.
	if _, err := f.svc.Login(ctx, ip, "admin", adminPassword); err != nil {
		t.Fatalf("correct credentials from a noisy ip must succeed: %v", err)
	}
}

// countingComparer counts password comparisons.
type countingComparer struct{ n atomic.Int32 }

func (c *countingComparer) compare(hash, pw []byte) error {
	c.n.Add(1)
	return bcrypt.CompareHashAndPassword(hash, pw)
}

func TestParallelWrongLoginsCannotExceedTheBudget(t *testing.T) {
	cmp := &countingComparer{}
	f := newFixture(t, auth.WithPasswordComparer(cmp.compare))
	f.setup(t)
	ctx := context.Background()
	cmp.n.Store(0)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Retry while only a concurrency cap refused the attempt, so every
			// goroutine ends with a definite failure or a lockout.
			for {
				_, err := f.svc.Login(ctx, ip, "admin", "wrong password!!")
				var throttled *auth.ThrottledError
				if errors.As(err, &throttled) && throttled.RetryAfter <= time.Second {
					continue
				}
				if !errors.Is(err, auth.ErrInvalidCredentials) && !errors.Is(err, auth.ErrThrottled) {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
		}()
	}
	wg.Wait()
	if n := cmp.n.Load(); n != auth.FreeAttempts {
		t.Fatalf("%d parallel attempts reached bcrypt; want exactly %d", n, auth.FreeAttempts)
	}
	if _, err := f.svc.Login(ctx, ip, "admin", adminPassword); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("pair must be locked after the budget: %v", err)
	}
}

func TestNoisyNeighboursNeverLockOutACorrectLogin(t *testing.T) {
	// Slow comparisons keep the bogus logins in flight while the admin logs in.
	slow := func(hash, pw []byte) error {
		time.Sleep(20 * time.Millisecond)
		return bcrypt.CompareHashAndPassword(hash, pw)
	}
	f := newFixture(t, auth.WithPasswordComparer(slow), auth.WithCompareConcurrency(2, 10*time.Second))
	f.setup(t)
	ctx := context.Background()
	for round := range 3 {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = f.svc.Login(ctx, ip, fmt.Sprintf("bogus-%d-%d", round, i), "wrong password!!")
			}()
		}
		close(start)
		time.Sleep(5 * time.Millisecond) // let the bogus requests reserve their slots
		if _, err := f.svc.Login(ctx, ip, "admin", adminPassword); err != nil {
			t.Fatalf("round %d: correct login next to 16 parallel bogus ones = %v", round, err)
		}
		wg.Wait()
	}
}

func TestComparisonSlotWaitIsBounded(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	blocking := func(hash, pw []byte) error {
		<-block
		return bcrypt.CompareHashAndPassword(hash, pw)
	}
	f := newFixture(t, auth.WithPasswordComparer(blocking), auth.WithCompareConcurrency(1, 50*time.Millisecond))
	f.setup(t)
	go func() { _, _ = f.svc.Login(context.Background(), "198.51.100.1", "admin", "whatever password") }()
	time.Sleep(20 * time.Millisecond)
	_, err := f.svc.Login(context.Background(), "198.51.100.2", "admin", adminPassword)
	var throttled *auth.ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("login while every slot is busy = %v; want a bounded wait and ThrottledError", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.svc.Login(ctx, "198.51.100.3", "admin", adminPassword); !errors.Is(err, context.Canceled) {
		t.Fatalf("login with a cancelled context = %v", err)
	}
}

func TestLoginPerformsOneComparisonRegardlessOfUserExistence(t *testing.T) {
	cmp := &countingComparer{}
	f := newFixture(t, auth.WithPasswordComparer(cmp.compare))
	f.setup(t)
	ctx := context.Background()
	for _, tc := range []struct {
		user, password string
		want           int32
	}{
		{"admin", "wrong password!!", 1},
		{"nobody", "wrong password!!", 1},
		{"admin", adminPassword, 1},
		{"admin", strings.Repeat("x", auth.MaxPasswordBytes+1), 0},
		{"nobody", strings.Repeat("x", auth.MaxPasswordBytes+1), 0},
	} {
		cmp.n.Store(0)
		_, _ = f.svc.Login(ctx, "198.51.100."+string(rune('1'+len(tc.user))), tc.user, tc.password)
		if got := cmp.n.Load(); got != tc.want {
			t.Errorf("login %s/%d bytes: %d comparisons; want %d", tc.user, len(tc.password), got, tc.want)
		}
	}
	if _, err := f.svc.Login(ctx, "203.0.113.9", "admin", strings.Repeat("x", 100)); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("over-long password: %v; want the generic failure", err)
	}
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()

	// Idle timeout.
	f.clock.Advance(auth.DefaultIdleTimeout - time.Minute)
	f.session(t, res.Token) // activity resets the idle timer
	f.clock.Advance(auth.DefaultIdleTimeout - time.Minute)
	f.session(t, res.Token)
	f.clock.Advance(auth.DefaultIdleTimeout)
	if _, err := f.svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("idle session: %v; want ErrUnauthenticated", err)
	}
	if _, err := f.store.GetSession(ctx, auth.HashToken(res.Token)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("expired session must be deleted: %v", err)
	}

	// Absolute timeout even with continuous activity.
	login, err := f.svc.Login(ctx, ip, "admin", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed < auth.DefaultAbsoluteTimeout-2*time.Hour; elapsed += time.Hour {
		f.clock.Advance(time.Hour)
		f.session(t, login.Token)
	}
	f.clock.Advance(2 * time.Hour)
	if _, err := f.svc.AuthenticateSession(ctx, login.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("session past its absolute lifetime: %v", err)
	}

	for _, tok := range []string{"", "forged-token"} {
		if _, err := f.svc.AuthenticateSession(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
}

func TestSessionTokensAreStoredHashed(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	if _, err := f.store.GetSession(context.Background(), res.Token); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal("the plaintext token must not be a lookup key")
	}
	sess, err := f.store.GetSession(context.Background(), auth.HashToken(res.Token))
	if err != nil || sess.CSRFToken != res.CSRFToken {
		t.Fatalf("session by hash = %+v, %v", sess, err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	if err := f.svc.Logout(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("session after logout: %v", err)
	}
	if err := f.svc.Logout(ctx, res.Token); err != nil {
		t.Fatalf("second logout: %v", err)
	}
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	f := newFixture(t)
	current := f.setup(t)
	ctx := context.Background()
	other, err := f.svc.Login(ctx, "198.51.100.7", "admin", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	actor := f.session(t, current.Token)

	const newPassword = "an even better passphrase"
	if err = f.svc.ChangePassword(ctx, actor, actor.User.ID, "wrong current", newPassword); !errors.Is(err, auth.ErrCurrentPassword) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err = f.svc.ChangePassword(ctx, actor, actor.User.ID, adminPassword, "short"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("weak new password: %v", err)
	}
	if err = f.svc.ChangePassword(ctx, actor, actor.User.ID, adminPassword, newPassword); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	f.session(t, current.Token) // the acting session survives
	if _, err = f.svc.AuthenticateSession(ctx, other.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("other session after password change: %v", err)
	}
	if _, err = f.svc.Login(ctx, ip, "admin", adminPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err = f.svc.Login(ctx, ip, "admin", newPassword); err != nil {
		t.Fatalf("new password: %v", err)
	}

	// Resetting another user's password needs no current password and revokes all of
	// that user's sessions.
	bob, err := f.svc.CreateUser(ctx, actor, "bob", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	bobSession, err := f.svc.Login(ctx, ip, "bob", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangePassword(ctx, actor, bob.ID, "", newPassword); err != nil {
		t.Fatalf("admin reset of another user: %v", err)
	}
	if _, err := f.svc.AuthenticateSession(ctx, bobSession.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("bob's session after reset: %v", err)
	}
	if err := f.svc.ChangePassword(ctx, actor, "usr_missing", "", newPassword); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestUserManagement(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	admin := f.session(t, res.Token)

	if _, err := f.svc.CreateUser(ctx, admin, "Admin", adminPassword); !errors.Is(err, auth.ErrUserExists) {
		t.Fatalf("duplicate username (case-insensitive): %v", err)
	}
	for _, bad := range []string{"", "has space", "semi;colon", strings.Repeat("a", 65)} {
		if _, err := f.svc.CreateUser(ctx, admin, bad, adminPassword); !errors.Is(err, auth.ErrInvalidUsername) {
			t.Errorf("username %q: %v", bad, err)
		}
	}
	if _, err := f.svc.CreateUser(ctx, admin, "carol", strings.Repeat("x", auth.MaxPasswordBytes+1)); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("password over 72 bytes: %v", err)
	}
	carol, err := f.svc.CreateUser(ctx, admin, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	users, err := f.svc.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers = %d, %v", len(users), err)
	}
	raw, _ := json.Marshal(users)
	if strings.Contains(string(raw), "$2a$") || strings.Contains(string(raw), "password") {
		t.Fatalf("user JSON leaks the password hash: %s", raw)
	}

	if err = f.svc.DeleteUser(ctx, admin, admin.User.ID); !errors.Is(err, auth.ErrDeleteSelf) {
		t.Fatalf("delete self: %v", err)
	}
	carolSession, err := f.svc.Login(ctx, ip, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteUser(ctx, admin, carol.ID); err != nil {
		t.Fatalf("delete carol: %v", err)
	}
	if _, err := f.svc.AuthenticateSession(ctx, carolSession.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("deleted user's session: %v", err)
	}
	if err := f.svc.DeleteUser(ctx, admin, carol.ID); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	// The static API key has no user, so only the last-user rule stops it.
	static := &auth.Principal{Method: auth.MethodAPIKey}
	if err := f.svc.DeleteUser(ctx, static, admin.User.ID); !errors.Is(err, auth.ErrLastUser) {
		t.Fatalf("delete last user: %v", err)
	}
}

func TestAPIKeys(t *testing.T) {
	const static = "static-env-key-0123456789"
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	admin := f.session(t, res.Token)

	if _, _, err := f.svc.CreateAPIKey(ctx, admin, "  "); !errors.Is(err, auth.ErrInvalidName) {
		t.Fatalf("empty name: %v", err)
	}
	k, plain, err := f.svc.CreateAPIKey(ctx, admin, "ci pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "mr_"+k.Prefix+"_") || len(plain) != len("mr_")+8+1+32 || k.CreatedBy != admin.User.ID {
		t.Fatalf("key %q / %+v has the wrong shape", plain, k)
	}

	// Only the hash is stored and nothing lists the plaintext.
	stored, err := f.store.GetAPIKeyByPrefix(ctx, k.Prefix)
	if err != nil || stored.Hash != auth.HashToken(plain) || strings.Contains(stored.Hash, plain) {
		t.Fatalf("stored key = %+v, %v", stored, err)
	}
	list, _ := f.svc.ListAPIKeys(ctx)
	raw, _ := json.Marshal(list)
	if len(list) != 1 || strings.Contains(string(raw), plain[len("mr_"+k.Prefix+"_"):]) || strings.Contains(string(raw), stored.Hash) {
		t.Fatalf("listing leaks key material: %s", raw)
	}

	p, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil || p.Method != auth.MethodAPIKey || p.APIKeyID != k.ID || p.UserID() != admin.User.ID {
		t.Fatalf("AuthenticateAPIKey = %+v, %v", p, err)
	}
	used, _ := f.store.GetAPIKeyByPrefix(ctx, k.Prefix)
	if used.LastUsedAt == nil || !used.LastUsedAt.Equal(f.clock.Now()) {
		t.Fatalf("last_used_at = %v; want %v", used.LastUsedAt, f.clock.Now())
	}
	f.clock.Advance(2 * time.Minute)
	if _, err = f.svc.AuthenticateAPIKey(ctx, plain); err != nil {
		t.Fatal(err)
	}
	used, _ = f.store.GetAPIKeyByPrefix(ctx, k.Prefix)
	if !used.LastUsedAt.Equal(f.clock.Now()) {
		t.Fatalf("last_used_at not refreshed: %v", used.LastUsedAt)
	}

	tampered := plain[:len(plain)-1] + map[bool]string{true: "a", false: "b"}[plain[len(plain)-1] != 'a']
	for _, bad := range []string{"", "mr_short", tampered, strings.ToUpper(plain), static + "x"} {
		if _, err = f.svc.AuthenticateAPIKey(ctx, bad); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("key %q: %v; want ErrUnauthenticated", bad, err)
		}
	}
	// A key imported from the deprecated MONGORESCUE_API_KEY works once imported.
	if _, err = f.svc.AuthenticateAPIKey(ctx, static); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("unknown legacy key = %v; want ErrUnauthenticated", err)
	}
	created, err := f.svc.ImportAPIKey(ctx, static)
	if err != nil || !created {
		t.Fatalf("ImportAPIKey = %v, %v", created, err)
	}
	if created, err = f.svc.ImportAPIKey(ctx, static); err != nil || created {
		t.Fatalf("second ImportAPIKey = %v, %v; want a no-op", created, err)
	}
	if _, err = f.svc.ImportAPIKey(ctx, "short"); err == nil {
		t.Fatal("a short legacy key must be refused")
	}
	imported, err := f.svc.AuthenticateAPIKey(ctx, static)
	if err != nil || imported.User != nil || imported.APIKeyID == "" {
		t.Fatalf("imported key = %+v, %v", imported, err)
	}
	if err := f.svc.DeleteAPIKey(ctx, admin, imported.APIKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AuthenticateAPIKey(ctx, static); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked imported key = %v", err)
	}

	if err := f.svc.DeleteAPIKey(ctx, admin, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AuthenticateAPIKey(ctx, plain); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("deleted key: %v", err)
	}
	if err := f.svc.DeleteAPIKey(ctx, admin, k.ID); !errors.Is(err, auth.ErrAPIKeyNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestDeletingAUserRevokesTheirAPIKeys(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	admin := f.session(t, res.Token)
	carol, err := f.svc.CreateUser(ctx, admin, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	carolLogin, err := f.svc.Login(ctx, ip, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	carolP := f.session(t, carolLogin.Token)
	_, carolKey, err := f.svc.CreateAPIKey(ctx, carolP, "carol's script")
	if err != nil {
		t.Fatal(err)
	}
	_, adminKey, err := f.svc.CreateAPIKey(ctx, admin, "admin's script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.AuthenticateAPIKey(ctx, carolKey); err != nil {
		t.Fatalf("carol's key before deletion: %v", err)
	}

	if err = f.svc.DeleteUser(ctx, admin, carol.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.AuthenticateAPIKey(ctx, carolKey); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("key of a deleted user: %v; want ErrUnauthenticated", err)
	}
	keys, err := f.svc.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].CreatedBy != admin.User.ID {
		t.Fatalf("keys after deleting carol = %+v, %v; want only the admin's", keys, err)
	}
	if _, err := f.svc.AuthenticateAPIKey(ctx, adminKey); err != nil {
		t.Fatalf("other users' keys must keep working: %v", err)
	}
}

// orphanKeyRepo hides the creator of every API key, as if a key had outlived its user.
type orphanKeyRepo struct{ auth.Repository }

func (orphanKeyRepo) GetUser(context.Context, string) (*auth.User, error) {
	return nil, auth.ErrUserNotFound
}

func TestAPIKeyOfAMissingCreatorIsRefused(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	_, plain, err := f.svc.CreateAPIKey(ctx, f.session(t, res.Token), "orphan")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(orphanKeyRepo{f.store}, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := svc.AuthenticateAPIKey(ctx, plain); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("orphaned key = %+v, %v; want ErrUnauthenticated", p, err)
	}
}

func TestSessionPolicyAppliesToExistingSessions(t *testing.T) {
	idle, absolute := time.Hour, 24*time.Hour
	var mu sync.Mutex
	f := newFixture(t, auth.WithSessionPolicy(func() (time.Duration, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return idle, absolute
	}))
	res := f.setup(t)
	ctx := context.Background()
	if res.ExpiresAt.Sub(f.clock.Now()) != absolute {
		t.Fatalf("expires in %v; want %v", res.ExpiresAt.Sub(f.clock.Now()), absolute)
	}
	f.clock.Advance(30 * time.Minute)
	if _, err := f.svc.AuthenticateSession(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	// Shortening the absolute lifetime ends sessions older than the new limit.
	mu.Lock()
	absolute = 20 * time.Minute
	mu.Unlock()
	if _, err := f.svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("session older than the new absolute limit = %v", err)
	}
}

func TestCheckCSRF(t *testing.T) {
	session := &auth.Principal{Method: auth.MethodSession, CSRFToken: "tok"}
	key := &auth.Principal{Method: auth.MethodAPIKey}
	for _, tc := range []struct {
		p      *auth.Principal
		method string
		token  string
		ok     bool
	}{
		{session, http.MethodGet, "", true},
		{session, http.MethodHead, "", true},
		{session, http.MethodPost, "", false},
		{session, http.MethodPut, "wrong", false},
		{session, http.MethodPatch, "tok", true},
		{session, http.MethodDelete, "tok", true},
		{key, http.MethodDelete, "", true},
		{nil, http.MethodPost, "", true},
	} {
		err := auth.CheckCSRF(tc.p, tc.method, tc.token)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, auth.ErrCSRF)) {
			t.Errorf("CheckCSRF(%v, %s, %q) = %v", tc.p != nil && tc.p.Method == auth.MethodSession, tc.method, tc.token, err)
		}
	}
}

func TestPrincipalContext(t *testing.T) {
	if auth.PrincipalFrom(context.Background()) != nil {
		t.Fatal("empty context must carry no principal")
	}
	p := &auth.Principal{Method: auth.MethodAPIKey}
	if got := auth.PrincipalFrom(auth.WithPrincipal(context.Background(), p)); got != p {
		t.Fatal("principal not round-tripped")
	}
	if (*auth.Principal)(nil).UserID() != "" || p.UserID() != "" {
		t.Fatal("UserID without a user must be empty")
	}
}
