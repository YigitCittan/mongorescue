package models

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// CredentialSource tells where the credentials of a connection (its user name
// and password) or of an S3 storage target (its access keys) come from.
type CredentialSource string

// Credential sources.
const (
	// CredentialsStored (also the empty value) keeps the credentials in the
	// store, sealed with the secret key: in the connection string, or in the
	// target's access key fields.
	CredentialsStored CredentialSource = "stored"
	// CredentialsVault reads them from HashiCorp Vault when they are used (see
	// VaultRef); nothing read from Vault is ever stored.
	CredentialsVault CredentialSource = "vault"
)

// VaultEngine is the Vault secrets engine credentials are read from.
type VaultEngine string

// Vault secrets engines.
const (
	// VaultKV2 reads static credentials from two fields of a KV version 2
	// secret.
	VaultKV2 VaultEngine = "kv-v2"
	// VaultDatabase issues a MongoDB user of a role of the database secrets
	// engine, with a lease (connections only).
	VaultDatabase VaultEngine = "database"
	// VaultAWS issues access keys of a role of the AWS secrets engine, with a
	// lease (S3 storage targets only).
	VaultAWS VaultEngine = "aws"
)

// VaultUse is what a VaultRef supplies credentials for.
type VaultUse string

// Vault uses.
const (
	// VaultForConnection supplies the user name and password of a connection.
	VaultForConnection VaultUse = "connection"
	// VaultForS3 supplies the access keys of an S3 storage target.
	VaultForS3 VaultUse = "s3"
)

// Default field names of KV v2 secrets.
const (
	// DefaultVaultUsernameField and DefaultVaultPasswordField name the user name
	// and password of a connection.
	DefaultVaultUsernameField = "username"
	DefaultVaultPasswordField = "password" //nolint:gosec // G101: a field name, not a credential.
	// DefaultVaultAccessKeyField and DefaultVaultSecretKeyField name the access
	// key pair of an S3 target (the names the AWS secrets engine uses).
	DefaultVaultAccessKeyField = "access_key"
	DefaultVaultSecretKeyField = "secret_key" //nolint:gosec // G101: a field name, not a credential.
)

// CredentialHolder keeps the credentials of a connection valid while a run uses
// them (implemented by connections.Service.Hold). Hold returns uri with the
// credentials to use, a context derived from ctx that ends with a cause when
// they can no longer be kept valid, and release, which the run calls when it
// ends. Connections whose credentials need no holding get ctx and uri back.
type CredentialHolder interface {
	Hold(ctx context.Context, connectionID, uri string) (context.Context, string, func(), error)
}

// ErrInvalidVaultRef is returned for an incomplete or malformed Vault reference.
var ErrInvalidVaultRef = errors.New("invalid vault reference")

// VaultRef names credentials in Vault. It holds no secret: it is stored and
// returned to API clients as it is.
type VaultRef struct {
	// Engine is the secrets engine: VaultKV2, VaultDatabase or VaultAWS.
	Engine VaultEngine `json:"engine"`
	// Mount is the mount path of the engine (default "secret" for kv-v2,
	// "database" and "aws" for the others), without the /v1/ prefix.
	Mount string `json:"mount,omitempty"`
	// Path is the secret path of a kv-v2 secret, below the mount (without
	// "data/").
	Path string `json:"path,omitempty"`
	// Role is the role of a dynamic engine (database, aws).
	Role string `json:"role,omitempty"`
	// UsernameField and PasswordField name the fields of a kv-v2 secret holding
	// a connection's user name and password (default username and password).
	UsernameField string `json:"username_field,omitempty"`
	PasswordField string `json:"password_field,omitempty"`
	// AccessKeyField and SecretKeyField name the fields of a kv-v2 secret
	// holding an S3 target's key pair (default access_key and secret_key);
	// SessionTokenField optionally names a session token.
	AccessKeyField    string `json:"access_key_field,omitempty"`
	SecretKeyField    string `json:"secret_key_field,omitempty"`
	SessionTokenField string `json:"session_token_field,omitempty"`
}

// IsZero reports whether r names nothing.
func (r VaultRef) IsZero() bool { return r == VaultRef{} }

// Dynamic reports whether r issues leased credentials (database, aws).
func (r *VaultRef) Dynamic() bool {
	return r != nil && (r.Engine == VaultDatabase || r.Engine == VaultAWS)
}

// Key identifies the credentials r names (equal references share a cache
// entry and a lease).
func (r *VaultRef) Key() string {
	if r == nil {
		return ""
	}
	return strings.Join([]string{string(r.Engine), r.Mount, r.Path, r.Role, r.UsernameField, r.PasswordField,
		r.AccessKeyField, r.SecretKeyField, r.SessionTokenField}, "\x00")
}

// String describes r for messages ("kv-v2 secret/data/mongo", "database role
// database/creds/backup"). It holds no secret.
func (r *VaultRef) String() string {
	switch {
	case r == nil:
		return ""
	case r.Engine == VaultKV2:
		return fmt.Sprintf("kv-v2 secret %s/data/%s", r.Mount, r.Path)
	default:
		return fmt.Sprintf("%s role %s/creds/%s", r.Engine, r.Mount, r.Role)
	}
}

// Limits of a Vault reference.
const (
	maxVaultPathLength  = 512
	maxVaultFieldLength = 128
)

var (
	// vaultSegment is one segment of a mount, path or role: no "." or "..", no
	// characters that change the meaning of the API path.
	vaultSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@+=-]*$`)
	// vaultField is the name of a kv-v2 field.
	vaultField = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// reservedMounts are Vault's own mounts, never a secrets engine of credentials.
var reservedMounts = []string{"sys", "auth", "identity", "cubbyhole"}

// Normalize trims r, applies the defaults of its engine and use, and checks it:
// a connection reads kv-v2 or database, an S3 target kv-v2 or aws.
func (r *VaultRef) Normalize(use VaultUse) error {
	if r == nil {
		return fmt.Errorf("%w: vault is required with credential_source vault", ErrInvalidVaultRef)
	}
	for _, p := range []*string{&r.Mount, &r.Path, &r.Role, &r.UsernameField, &r.PasswordField, &r.AccessKeyField, &r.SecretKeyField, &r.SessionTokenField} {
		*p = strings.TrimSpace(*p)
	}
	r.Engine = VaultEngine(strings.ToLower(strings.TrimSpace(string(r.Engine))))
	r.Mount, r.Path = strings.Trim(r.Mount, "/"), strings.Trim(r.Path, "/")
	switch {
	case r.Engine == VaultKV2:
		r.Mount = orDefaultString(r.Mount, "secret")
	case r.Engine == VaultDatabase && use == VaultForConnection:
		r.Mount = orDefaultString(r.Mount, "database")
	case r.Engine == VaultAWS && use == VaultForS3:
		r.Mount = orDefaultString(r.Mount, "aws")
	case use == VaultForConnection:
		return fmt.Errorf("%w: engine must be %s or %s for a connection", ErrInvalidVaultRef, VaultKV2, VaultDatabase)
	default:
		return fmt.Errorf("%w: engine must be %s or %s for an S3 storage target", ErrInvalidVaultRef, VaultKV2, VaultAWS)
	}
	if err := checkVaultPath("mount", r.Mount, true); err != nil {
		return err
	}
	if r.Engine == VaultKV2 {
		if r.Role != "" {
			return fmt.Errorf("%w: role applies to the %s and %s engines only", ErrInvalidVaultRef, VaultDatabase, VaultAWS)
		}
		if err := checkVaultPath("path", r.Path, true); err != nil {
			return err
		}
		return r.normalizeFields(use)
	}
	if r.Path != "" {
		return fmt.Errorf("%w: path applies to the %s engine only; name the role instead", ErrInvalidVaultRef, VaultKV2)
	}
	if err := checkVaultPath("role", r.Role, false); err != nil {
		return err
	}
	if r.UsernameField != "" || r.PasswordField != "" || r.AccessKeyField != "" || r.SecretKeyField != "" || r.SessionTokenField != "" {
		return fmt.Errorf("%w: field names apply to the %s engine only", ErrInvalidVaultRef, VaultKV2)
	}
	return nil
}

// normalizeFields applies and checks the field names of a kv-v2 reference.
func (r *VaultRef) normalizeFields(use VaultUse) error {
	var fields []*string
	if use == VaultForConnection {
		if r.AccessKeyField != "" || r.SecretKeyField != "" || r.SessionTokenField != "" {
			return fmt.Errorf("%w: a connection names username_field and password_field", ErrInvalidVaultRef)
		}
		r.UsernameField = orDefaultString(r.UsernameField, DefaultVaultUsernameField)
		r.PasswordField = orDefaultString(r.PasswordField, DefaultVaultPasswordField)
		fields = []*string{&r.UsernameField, &r.PasswordField}
	} else {
		if r.UsernameField != "" || r.PasswordField != "" {
			return fmt.Errorf("%w: an S3 storage target names access_key_field and secret_key_field", ErrInvalidVaultRef)
		}
		r.AccessKeyField = orDefaultString(r.AccessKeyField, DefaultVaultAccessKeyField)
		r.SecretKeyField = orDefaultString(r.SecretKeyField, DefaultVaultSecretKeyField)
		fields = []*string{&r.AccessKeyField, &r.SecretKeyField}
		if r.SessionTokenField != "" {
			fields = append(fields, &r.SessionTokenField)
		}
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if len(*f) > maxVaultFieldLength || !vaultField.MatchString(*f) {
			return fmt.Errorf("%w: field name %q must be letters, digits, '_', '.' or '-'", ErrInvalidVaultRef, *f)
		}
		if seen[*f] {
			return fmt.Errorf("%w: field %q is named twice", ErrInvalidVaultRef, *f)
		}
		seen[*f] = true
	}
	return nil
}

// checkVaultPath checks a mount, path (multi-segment) or role (one segment).
func checkVaultPath(what, v string, multi bool) error {
	if v == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidVaultRef, what)
	}
	if len(v) > maxVaultPathLength {
		return fmt.Errorf("%w: %s must be at most %d characters", ErrInvalidVaultRef, what, maxVaultPathLength)
	}
	segs := strings.Split(v, "/")
	if !multi && len(segs) > 1 {
		return fmt.Errorf("%w: %s must not contain '/'", ErrInvalidVaultRef, what)
	}
	for _, s := range segs {
		if !vaultSegment.MatchString(s) {
			return fmt.Errorf("%w: %s %q must be '/'-separated segments of letters, digits and _ . @ + = - (no empty, '.' or '..' segment)",
				ErrInvalidVaultRef, what, v)
		}
	}
	if what == "mount" {
		for _, m := range reservedMounts {
			if strings.EqualFold(segs[0], m) {
				return fmt.Errorf("%w: mount %q is a Vault system path, not a secrets engine", ErrInvalidVaultRef, v)
			}
		}
	}
	return nil
}

// orDefaultString returns v, or def when v is empty.
func orDefaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// NormalizeCredentialSource returns the source of s ("" and "stored" are
// CredentialsStored) or an error for an unknown one.
func NormalizeCredentialSource(s CredentialSource) (CredentialSource, error) {
	switch CredentialSource(strings.ToLower(strings.TrimSpace(string(s)))) {
	case "", CredentialsStored:
		return CredentialsStored, nil
	case CredentialsVault:
		return CredentialsVault, nil
	}
	return "", fmt.Errorf("%w: credential_source must be %s or %s", ErrInvalidVaultRef, CredentialsStored, CredentialsVault)
}
