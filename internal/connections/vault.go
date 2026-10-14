package connections

import (
	"context"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// ErrCredentials is returned when the credentials of a connection read from
// Vault cannot be resolved; it wraps the reason (vaultcreds.ErrUnavailable,
// vaultcreds.ErrRefused, ...).
var ErrCredentials = errors.New("connections: credentials from Vault")

// Credentials resolves the user name and password of connections whose
// credentials come from Vault (implemented by *vaultcreds.Resolver).
type Credentials interface {
	// Configured reports whether a Vault is configured.
	Configured() bool
	// Connection returns the credentials of ref for a short use.
	Connection(ctx context.Context, ref models.VaultRef) (vaultcreds.UserPassword, error)
	// Hold returns the credentials of ref for a run and keeps them valid until
	// release; the context ends when they can no longer be kept valid.
	Hold(ctx context.Context, ref models.VaultRef) (context.Context, vaultcreds.UserPassword, func(), error)
}

// WithCredentials sets the resolver of credentials read from Vault. Without one,
// connections with credential_source vault cannot be saved or used.
func WithCredentials(c Credentials) Option { return func(s *Service) { s.creds = c } }

// CredentialInput is the client-editable credential source of a connection. An
// omitted source keeps the stored one.
type CredentialInput struct {
	// CredentialSource is models.CredentialsStored or models.CredentialsVault.
	CredentialSource *models.CredentialSource `json:"credential_source,omitempty"`
	// Vault names the credentials in Vault (with CredentialsVault); omitted keeps
	// the stored reference.
	Vault *models.VaultRef `json:"vault,omitempty"`
}

// applyCredentials applies in to c and checks the result: a connection reading
// its credentials from Vault needs a configured Vault, a valid reference and a
// URI without user information. It reports whether the source or reference
// changed.
func (s *Service) applyCredentials(in CredentialInput, c *models.Connection) (bool, error) {
	before := *c
	if in.CredentialSource != nil {
		c.CredentialSource = *in.CredentialSource
	}
	if in.Vault != nil {
		c.Vault = *in.Vault
	}
	src, err := models.NormalizeCredentialSource(c.CredentialSource)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if src == models.CredentialsStored {
		// Stored as "", so connections saved before Vault support compare equal.
		c.CredentialSource, c.Vault = "", models.VaultRef{}
		return before.CredentialSource != "" || !before.Vault.IsZero(), nil
	}
	c.CredentialSource = src
	if err = c.Vault.Normalize(models.VaultForConnection); err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if s.creds == nil || !s.creds.Configured() {
		return false, fmt.Errorf("%w: %w", ErrInvalid, vaultcreds.ErrNotConfigured)
	}
	if mongouri.HasUserinfo(c.URI) {
		return false, fmt.Errorf("%w: with credential_source vault the uri must not contain a user name or password; remove them, Vault supplies both", ErrInvalid)
	}
	return before.CredentialSource != c.CredentialSource || before.Vault != c.Vault, nil
}

// withCredentials returns c with the credentials read from Vault in its URI
// when its source is Vault, for a short use; c itself otherwise.
func (s *Service) withCredentials(ctx context.Context, c *models.Connection) (*models.Connection, error) {
	if !c.FromVault() {
		return c, nil
	}
	if s.creds == nil {
		return nil, fmt.Errorf("%w for connection %s: %w", ErrCredentials, c.Name, vaultcreds.ErrNotConfigured)
	}
	up, err := s.creds.Connection(ctx, c.Vault)
	if err != nil {
		return nil, fmt.Errorf("%w for connection %s: %w", ErrCredentials, c.Name, err)
	}
	out := *c
	out.URI = mongouri.WithUserinfo(c.URI, up.Username, up.Password)
	return &out, nil
}

// forUse returns connection id with the credentials read from Vault in its URI.
func (s *Service) forUse(ctx context.Context, id string) (*models.Connection, error) {
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withCredentials(ctx, c)
}

// Hold keeps the credentials of connection id valid for a run: for credentials
// issued by the database secrets engine of Vault, it returns uri with the
// credentials of a lease that is renewed until release is called (and then
// revoked), and a context derived from ctx that ends, with a cause wrapping
// vaultcreds.ErrLeaseLost, when the lease can no longer be kept valid. For any
// other connection it returns ctx and uri unchanged. uri is the connection's
// URI as the run uses it (with its read preference); its user information is
// replaced. The returned URI must never be logged or stored.
func (s *Service) Hold(ctx context.Context, id, uri string) (context.Context, string, func(), error) {
	noop := func() {}
	if id == "" {
		return ctx, uri, noop, nil
	}
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return ctx, uri, noop, err
	}
	if !c.FromVault() || !c.Vault.Dynamic() {
		return ctx, uri, noop, nil
	}
	if s.creds == nil {
		return ctx, uri, noop, fmt.Errorf("%w for connection %s: %w", ErrCredentials, c.Name, vaultcreds.ErrNotConfigured)
	}
	held, up, release, err := s.creds.Hold(ctx, c.Vault)
	if err != nil {
		return ctx, uri, noop, fmt.Errorf("%w for connection %s: %w", ErrCredentials, c.Name, err)
	}
	return held, mongouri.WithUserinfo(uri, up.Username, up.Password), release, nil
}
