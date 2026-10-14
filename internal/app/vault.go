package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/vault"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// vaultCheckTimeout bounds the check of the Vault authentication at startup.
const vaultCheckTimeout = 10 * time.Second

// openVaultResolver returns the resolver of credentials read from Vault: with
// MONGORESCUE_VAULT_ADDR, a client of that Vault authenticated like key wrapping
// (VAULT_TOKEN, AppRole or Kubernetes, from getenv); without it, a resolver that
// is not configured.
func openVaultResolver(cfg *config.Config, getenv func(string) string, logger *slog.Logger) (*vaultcreds.Resolver, error) {
	if !cfg.Vault.Enabled() {
		return vaultcreds.New(vaultcreds.Config{Logger: logger}), nil
	}
	auth, err := vault.AuthFromEnv(getenv)
	if err != nil {
		return nil, fmt.Errorf("%s is set: %w", config.EnvVaultAddr, err)
	}
	client, err := vault.New(vault.Config{Addr: cfg.Vault.Addr, Namespace: strings.TrimSpace(getenv(vault.EnvNamespace)), Auth: auth,
		CACert: strings.TrimSpace(getenv(vault.EnvCACert)), AllowInsecure: cfg.Vault.AllowInsecure})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvVaultAddr, err)
	}
	if strings.HasPrefix(client.Addr(), "http://") {
		logger.Warn("credentials are read from a Vault over plain http (" + config.EnvVaultAllowInsecure + "); use https outside development")
	}
	return vaultcreds.New(vaultcreds.Config{Client: client, CacheTTL: cfg.Vault.CacheTTL, Logger: logger}), nil
}

// vaultCounter counts the connections and targets that read credentials from
// Vault (implemented by *store.SQLiteStore).
type vaultCounter interface {
	CountVaultCredentials(ctx context.Context) (conns, targets int, err error)
}

// checkVaultUse refuses to start when connections or storage targets read their
// credentials from Vault but none is configured. With one configured and in use,
// it checks that Vault accepts MongoRescue's authentication; a failure is only
// logged (Vault may come up later; until then the runs that need it fail and
// alert).
func checkVaultUse(ctx context.Context, counter vaultCounter, resolver *vaultcreds.Resolver, logger *slog.Logger) error {
	conns, targets, err := counter.CountVaultCredentials(ctx)
	if err != nil {
		return err
	}
	if conns+targets == 0 {
		return nil
	}
	if !resolver.Configured() {
		return fmt.Errorf("%d connection(s) and %d storage target(s) read their credentials from Vault: %w", conns, targets, vaultcreds.ErrNotConfigured)
	}
	checkCtx, cancel := context.WithTimeout(ctx, vaultCheckTimeout)
	defer cancel()
	if err = resolver.Check(checkCtx); err != nil {
		logger.Warn("Vault does not accept MongoRescue's authentication yet; connections and storage targets with credentials from Vault fail until it does",
			logsafe.Error(err))
		return nil
	}
	logger.Info("credentials from Vault are configured", slog.Int("connections", conns), slog.Int("storage_targets", targets))
	return nil
}

// vaultAlert publishes a events.VaultUnavailable event with message.
func vaultAlert(bus *events.Bus) func(message string) {
	return func(message string) {
		bus.Publish(context.Background(), events.Event{Type: events.VaultUnavailable, Time: time.Now().UTC(), Status: "failed", Error: message})
	}
}

// openHeldOplogSession opens the collector session of stream st on connection
// conn, holding its credentials: a lease from the database secrets engine is
// renewed while the session lives and the session reports itself stale before
// the lease ends; static kv-v2 credentials make it stale once Vault holds others.
func openHeldOplogSession(ctx context.Context, connSvc *connections.Service, prober *mongoconn.Prober, st *pitr.Stream) (collector.Session, error) {
	conn, err := connSvc.Resolve(ctx, st.ConnectionID)
	if err != nil {
		return nil, err
	}
	held, uri, release, err := connSvc.Hold(ctx, st.ConnectionID, conn.URI)
	if err != nil {
		return nil, err
	}
	sess, err := openOplogSession(mongotls.NewContext(held, conn.TLS()), prober, uri, st.ReadPreference)
	if err != nil {
		release()
		return nil, err
	}
	if !conn.FromVault() {
		release()
		return sess, nil
	}
	return &heldSession{Session: sess, held: held, release: release, leased: conn.Vault.Dynamic(), uri: uri, id: st.ConnectionID, svc: connSvc}, nil
}

// heldSession is a collector session on credentials read from Vault.
type heldSession struct {
	collector.Session
	held    context.Context
	release func()
	// leased is set for credentials of a lease (kept by Hold).
	leased bool
	// uri is the connection string the session was opened with; id and svc
	// resolve the current one.
	uri string
	id  string
	svc *connections.Service
}

// Stale reports that the lease of the session's credentials ends soon or, for
// static credentials, that the connection now resolves to others (rotated in
// Vault, or a changed connection). A failure to resolve keeps the session.
func (s *heldSession) Stale(ctx context.Context) bool {
	if s.held.Err() != nil {
		return true
	}
	if s.leased {
		return false
	}
	cur, err := s.svc.Resolve(ctx, s.id)
	return err == nil && cur.URI != s.uri
}

// Close disconnects the session and releases its credentials.
func (s *heldSession) Close() {
	s.Session.Close()
	s.release()
}
