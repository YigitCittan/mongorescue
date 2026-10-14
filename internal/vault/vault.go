// Package vault is a minimal HTTP client of HashiCorp Vault, shared by the
// vault-transit key wrapper (internal/keywrap/provider) and the credentials read
// from Vault (internal/vaultcreds).
//
// It authenticates with a token, AppRole or the Kubernetes method (see
// AuthFromEnv), logs in again once when Vault refuses an expired login token,
// never follows a redirect (every request carries the token) and talks to
// Vault over https only, unless plain http is explicitly allowed for a
// development Vault. Neither the token nor any response body is ever part of
// an error or a log line.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Environment variables of the Vault authentication, shared by every Vault
// user (key wrapping and credentials).
const (
	// EnvToken is a Vault token (the simplest authentication).
	EnvToken = "VAULT_TOKEN" //nolint:gosec // G101: a variable name, not a credential.
	// EnvRoleID and EnvSecretID authenticate with AppRole.
	EnvRoleID   = "VAULT_ROLE_ID"
	EnvSecretID = "VAULT_SECRET_ID" //nolint:gosec // G101: a variable name, not a credential.
	// EnvAppRoleMount is the mount of the AppRole method (default approle).
	EnvAppRoleMount = "VAULT_APPROLE_MOUNT"
	// EnvK8sRole authenticates with the Kubernetes method as this role, with the
	// pod's service account token.
	EnvK8sRole = "VAULT_K8S_ROLE"
	// EnvK8sMount is the mount of the Kubernetes method (default kubernetes).
	EnvK8sMount = "VAULT_K8S_MOUNT"
	// EnvK8sTokenPath is the service account token file (default
	// /var/run/secrets/kubernetes.io/serviceaccount/token).
	EnvK8sTokenPath = "VAULT_K8S_TOKEN_PATH" //nolint:gosec // G101: a variable name, not a credential.
	// EnvNamespace is the Vault Enterprise namespace (X-Vault-Namespace).
	EnvNamespace = "VAULT_NAMESPACE"
	// EnvCACert is a PEM file of the CA certificates that verify Vault's TLS
	// certificate (default: the system roots).
	EnvCACert = "VAULT_CACERT"
)

// Authentication methods.
const (
	// AuthToken uses a fixed token (VAULT_TOKEN).
	AuthToken = "token"
	// AuthAppRole logs in with a role ID and a secret ID.
	AuthAppRole = "approle"
	// AuthKubernetes logs in with the pod's service account token.
	AuthKubernetes = "kubernetes"
)

// defaultK8sTokenPath is where Kubernetes mounts the service account token.
const defaultK8sTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: a path, not a credential.

// RequestTimeout bounds one HTTP request to Vault; the caller's context usually
// bounds it more tightly.
const RequestTimeout = 30 * time.Second

// maxResponseBody bounds a Vault response body.
const maxResponseBody = 1 << 20

// Sentinel errors.
var (
	// ErrNoAuth is returned when the environment configures no Vault
	// authentication.
	ErrNoAuth = errors.New("no Vault authentication: set " + EnvToken + ", " + EnvRoleID + " and " +
		EnvSecretID + " (AppRole), or " + EnvK8sRole + " (Kubernetes)")
	// ErrAuthConfig is returned when the configured authentication cannot work
	// (a service account token that cannot be read, a fixed token that Vault
	// refused): retrying does not help.
	ErrAuthConfig = errors.New("the Vault authentication cannot work")
	// ErrInvalidAddr is returned for a Vault address that is not
	// http(s)://host[:port] without a path, query or user information.
	ErrInvalidAddr = errors.New("the Vault address must be https://<host>:<port> (no path, query or credentials)")
	// ErrInsecureAddr is returned for a plain http:// address unless allowed.
	ErrInsecureAddr = errors.New("the Vault address uses plain http://, which would send the Vault token and the secrets unencrypted; use https://")
	// ErrRedirect describes a redirect Vault answered with. Redirects are never
	// followed: every request carries the token, which must not reach another
	// host.
	ErrRedirect = errors.New("the server answered with a redirect, which is not followed")
)

// Auth is how a Client obtains its token.
type Auth struct {
	// Method is AuthToken, AuthAppRole or AuthKubernetes.
	Method string
	// Token is the fixed token of AuthToken.
	Token string
	// Mount is the mount of the login method (AppRole, Kubernetes).
	Mount string
	// Role is the AppRole role ID or the Kubernetes role.
	Role string
	// SecretID is the AppRole secret ID.
	SecretID string
	// TokenPath is the Kubernetes service account token file.
	TokenPath string
}

// Configured reports whether an authentication method is set.
func (a Auth) Configured() bool { return a.Method != "" }

// AuthFromEnv returns the authentication of the environment: VAULT_TOKEN, else
// AppRole (VAULT_ROLE_ID and VAULT_SECRET_ID), else Kubernetes (VAULT_K8S_ROLE).
// It returns ErrNoAuth when none is set.
func AuthFromEnv(getenv func(string) string) (Auth, error) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	switch {
	case env(EnvToken) != "":
		return Auth{Method: AuthToken, Token: env(EnvToken)}, nil
	case env(EnvRoleID) != "" && env(EnvSecretID) != "":
		return Auth{Method: AuthAppRole, Mount: orDefault(env(EnvAppRoleMount), "approle"), Role: env(EnvRoleID), SecretID: env(EnvSecretID)}, nil
	case env(EnvK8sRole) != "":
		return Auth{Method: AuthKubernetes, Mount: orDefault(env(EnvK8sMount), "kubernetes"), Role: env(EnvK8sRole),
			TokenPath: orDefault(env(EnvK8sTokenPath), defaultK8sTokenPath)}, nil
	}
	return Auth{}, ErrNoAuth
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Config configures a Client.
type Config struct {
	// Addr is the Vault address, https://<host>:<port>.
	Addr string
	// Namespace is the Vault Enterprise namespace (empty for none).
	Namespace string
	// Auth is the authentication.
	Auth Auth
	// HTTPClient replaces the default client (tests); its redirects are never
	// followed either. CACert is ignored with it.
	HTTPClient *http.Client
	// CACert is a PEM file of the CA certificates that verify Vault's TLS
	// certificate; empty uses the system roots.
	CACert string
	// AllowInsecure accepts a plain http:// address (a development Vault).
	AllowInsecure bool
	// Name prefixes every error ("vault" when empty), such as "vault-transit".
	Name string
}

// Client calls the HTTP API of one Vault server. It is safe for concurrent use.
type Client struct {
	addr, namespace, name string
	auth                  Auth
	http                  *http.Client

	mu    sync.Mutex
	token string
}

// ParseAddr checks a Vault address and returns it normalised (scheme and host,
// no trailing slash). Plain http:// needs allowInsecure.
func ParseAddr(raw string, allowInsecure bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", ErrInvalidAddr
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure {
			return "", ErrInsecureAddr
		}
	default:
		return "", ErrInvalidAddr
	}
	return u.Scheme + "://" + u.Host, nil
}

// New returns a Client of cfg. It does not call Vault.
func New(cfg Config) (*Client, error) {
	name := orDefault(cfg.Name, "vault")
	addr, err := ParseAddr(cfg.Addr, cfg.AllowInsecure)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !cfg.Auth.Configured() {
		return nil, fmt.Errorf("%s: %w", name, ErrNoAuth)
	}
	c := &Client{addr: addr, namespace: strings.TrimSpace(cfg.Namespace), name: name, auth: cfg.Auth}
	switch {
	case cfg.HTTPClient != nil:
		cp := *cfg.HTTPClient
		cp.CheckRedirect = noRedirect
		c.http = &cp
	case strings.TrimSpace(cfg.CACert) != "":
		if c.http, err = clientWithCA(name, cfg.CACert); err != nil {
			return nil, err
		}
	default:
		c.http = &http.Client{Timeout: RequestTimeout, CheckRedirect: noRedirect}
	}
	if cfg.Auth.Method == AuthToken {
		c.token = cfg.Auth.Token
	}
	return c, nil
}

// Addr returns the Vault address.
func (c *Client) Addr() string { return c.addr }

// noRedirect is the CheckRedirect of every client: the 3xx response is returned
// as it is, and treated as an error.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// clientWithCA returns an HTTP client trusting the CA certificates of path.
func clientWithCA(name, path string) (*http.Client, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // G304: an operator-configured CA file.
	if err != nil {
		return nil, fmt.Errorf("%s: read %s: %w", name, EnvCACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: %s holds no PEM certificate", name, EnvCACert)
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected default transport", name)
	}
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: RequestTimeout, Transport: tr, CheckRedirect: noRedirect}, nil
}

// StatusError is a non-2xx answer of Vault. It never holds a secret: Vault's
// error messages name paths and policies.
type StatusError struct {
	// Path is the API path called.
	Path string
	// Status is the HTTP status.
	Status int
	// Messages are Vault's error messages.
	Messages []string
	name     string
}

// Error implements error.
func (e *StatusError) Error() string {
	msg := http.StatusText(e.Status)
	switch {
	case IsRedirect(e.Status):
		msg = ErrRedirect.Error()
	case len(e.Messages) > 0:
		msg = strings.Join(e.Messages, "; ")
	}
	return fmt.Sprintf("%s: %s: HTTP %d: %s", e.name, e.Path, e.Status, msg)
}

// Permanent reports whether retrying the request cannot help.
func (e *StatusError) Permanent() bool { return StatusPermanent(e.Status) }

// IsRedirect reports whether code is a redirect status.
func IsRedirect(code int) bool { return code >= 300 && code < 400 }

// StatusPermanent reports whether an HTTP status of Vault means a retry cannot
// help (a refused request, a missing path or permission, a redirect).
func StatusPermanent(code int) bool {
	if IsRedirect(code) {
		return true
	}
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// Do calls path ("/v1/...") with method and the current token, logging in first
// (AppRole, Kubernetes) and once more when the token was refused (it expired).
// body, when not nil, is sent as JSON; the answer is decoded into out when out
// is not nil and the answer has a body.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token == "" {
		var err error
		if token, err = c.login(ctx); err != nil {
			return err
		}
	}
	err := c.send(ctx, method, path, token, body, out)
	var se *StatusError
	if c.auth.Method != AuthToken && errors.As(err, &se) && se.Status == http.StatusForbidden {
		if token, err = c.login(ctx); err != nil {
			return err
		}
		err = c.send(ctx, method, path, token, body, out)
	}
	return err
}

// Login obtains a token now (AppRole, Kubernetes); with a fixed token it does
// nothing. It checks the authentication at startup.
func (c *Client) Login(ctx context.Context) error {
	if c.auth.Method == AuthToken {
		return nil
	}
	_, err := c.login(ctx)
	return err
}

// login obtains a token with AppRole or Kubernetes authentication.
func (c *Client) login(ctx context.Context) (string, error) {
	var body map[string]string
	switch c.auth.Method {
	case AuthAppRole:
		body = map[string]string{"role_id": c.auth.Role, "secret_id": c.auth.SecretID}
	case AuthKubernetes:
		jwt, err := os.ReadFile(c.auth.TokenPath)
		if err != nil {
			return "", fmt.Errorf("%s: %w: read the service account token: %w", c.name, ErrAuthConfig, err)
		}
		body = map[string]string{"role": c.auth.Role, "jwt": strings.TrimSpace(string(jwt))}
	default:
		return "", fmt.Errorf("%s: %w: the token was refused", c.name, ErrAuthConfig)
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := c.send(ctx, http.MethodPost, "/v1/auth/"+c.auth.Mount+"/login", "", body, &out); err != nil {
		return "", err
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("%s: the login returned no token", c.name)
	}
	c.mu.Lock()
	c.token = out.Auth.ClientToken
	c.mu.Unlock()
	return out.Auth.ClientToken, nil
}

// send calls path with token and decodes the response into out.
func (c *Client) send(ctx context.Context, method, path, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode request: %w", c.name, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, reader)
	if err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Vault-Request", "true")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", c.name, path, scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("%s: %s: read response: %w", c.name, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		return &StatusError{Path: path, Status: resp.StatusCode, Messages: e.Errors, name: c.name}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: %s: decode response: %w", c.name, path, err)
	}
	return nil
}

// scrubURLError returns err without the request URL (which holds no secret, but
// is repeated by the path already) when it is a *url.Error.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
