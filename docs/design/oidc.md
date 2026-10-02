# Design: single sign-on with OpenID Connect

Status: implemented in v0.18.0 (#54). Depends on [dashboard roles](roles.md).

Administrators can let people sign in through an OpenID Connect provider (Entra ID,
Google, Okta, Keycloak). Groups in the ID token decide the dashboard role. Local
accounts remain, and a local administrator is the break-glass way in when the
provider is down or misconfigured. API keys, the CLI and MCP are unchanged. Setup
recipes for each provider are in [sso.md](../sso.md).

## Dependency: audited libraries, not hand-written JOSE

The protocol work uses two libraries:

- [`github.com/coreos/go-oidc/v3`](https://github.com/coreos/go-oidc) **v3.21.0**:
  discovery, the JWKS cache with refetch on an unknown key ID, and ID token
  verification (signature, `iss`, `aud`, `exp`). It brings
  `github.com/go-jose/go-jose/v4` v4.1.4.
- [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) **v0.36.0**
  (already an indirect dependency, now direct): the authorization URL, the code
  exchange and PKCE (`GenerateVerifier`, `S256ChallengeOption`, `VerifierOption`).

Both are pinned exactly in `go.mod`. JOSE is where hand-written code goes wrong
(algorithm confusion, `alg=none`, key type mismatches, `kid` handling); go-oidc is
widely deployed, reviewed and maintained, and go-jose has had its own security
audits. The alternative, a few hundred lines of our own JWT and JWKS parsing, would
need the same review every time it changes. govulncheck reports nothing reachable.

The libraries are configured strictly:

- `oidc.Config{SupportedSigningAlgs: [RS256, ES256]}`: no `none`, no HMAC, nothing
  else. `SkipClientIDCheck`, `SkipExpiryCheck` and `SkipIssuerCheck` are never set,
  and `InsecureIssuerURLContext` is not used: discovery must report exactly the
  configured issuer.
- All provider HTTP goes through an `*http.Client` injected from `internal/app`:
  `notify.NewHTTPClient()` (10 second timeout, no redirects, refuses link-local and
  cloud metadata addresses also after DNS resolution). It is passed with
  `oidc.ClientContext`, which stores it under `oauth2.HTTPClient`, the key x/oauth2
  reads too. Its transport is wrapped so that every response body (discovery, JWKS,
  token) fails beyond 1 MiB. `internal/auth` does not import `notify`.
- The protocol glue (provider cache, authorization URL, exchange, verification,
  claim extraction) is `internal/auth/oidc`; it has no HTTP handlers and decides
  nothing about users or roles.

go-oidc refetches keys in a goroutine of its own (bounded by the client's timeout)
and caches them per provider; MongoRescue keeps one provider, for the configured
issuer, for an hour.

## Settings: the `oidc` section

`internal/settings/oidc.go`, modelled on the audit section. The client secret is a
`secret` key in `keyDefs`: sealed with secretbox at rest, masked as `******` in API
responses, kept when the mask is sent back.

| Key | Default | Notes |
| :--- | :--- | :--- |
| `enabled` | `false` | Turning it on validates the section and runs the discovery test |
| `display_name` | `Single sign-on` | Label of the sign-in button |
| `issuer` | — | `https`; `http` only for loopback. Entra `/common` and `/organizations` are refused: a tenant issuer is required |
| `client_id` | — | |
| `client_secret` | — | Secret. Empty means a public client (PKCE only) |
| `scopes` | `openid email profile` | `openid` is always added |
| `redirect_url` | required | Never derived from `Host`. The dashboard pre-fills `location.origin + "/auth/oidc/callback"`; the path must be exactly `/auth/oidc/callback` |
| `username_claim` | `preferred_username` | Falls back to `email`, then `sub`, then cleaned to the username characters |
| `groups_claim` | `groups` | A dot path (Keycloak `realm_access.roles`); the value is a string or a list of strings |
| `role_mappings` | `[]` | `{group, role}`, matched exactly, highest role wins, at most 100 |
| `default_role` | `""` (deny) | `viewer` or `operator` only. **admin is refused**: admin only comes from a group mapping |
| `allowed_email_domains` | `[]` (any) | Exact lower-case domain after the last `@`; requires `email_verified` to be the JSON boolean `true` |
| `auto_create_users` | `true` | When off, unknown subjects are denied (`no_role`) and audited. There is no approval queue |
| `local_login` | `all` | `all` or `admins_only` (break-glass: only local admins may use the password form) |
| `rp_logout` | `false` | Opt-in RP-initiated logout through `end_session_endpoint`, with `client_id` and no `id_token_hint` |

Checks the settings package cannot make itself run through a `settings.OIDCGuard`
that `internal/app` wires: turning `enabled` on, or `local_login` to `admins_only`,
needs at least one local administrator (`auth.Service.CheckOIDCChange`); turning
single sign-on on, or changing the issuer while it is on, fetches discovery and the
JWKS; switching `admins_only` on ends the sessions of local non-admins
(`auth.Service.ApplyOIDCChange`). The desktop app refuses to turn it on.

`POST /api/v1/settings/oidc/test` (admin) fetches discovery and the JWKS of the
given or stored issuer and reports the endpoints, the advertised and usable
algorithms, the PKCE methods and the number and types of keys, never secrets.

## Users: migration 0020

```sql
ALTER TABLE users ADD COLUMN auth_provider TEXT NOT NULL DEFAULT 'local' CHECK (auth_provider IN ('local','oidc'));
ALTER TABLE users ADD COLUMN subject TEXT;  -- iss + '#' + sub
CREATE UNIQUE INDEX users_by_subject ON users (subject) WHERE subject IS NOT NULL;
```

- Existing users become `local`. `password_hash` stays `NOT NULL`; OIDC users store
  `''`. The store enforces `oidc` ⇔ subject set ⇔ empty hash on every insert and
  refuses to give an OIDC user a password. The upgrade-compatibility test has a step
  for schema 20.
- Users are identified by `subject` only, never by email or username (the "nOAuth"
  class of account takeover). Accounts are never linked by name or email: a name
  collision fails with `account_conflict` and an administrator renames one of the
  two. A changed issuer means new users.
- `auth.User.AuthProvider` (`json:"auth_provider"`) is exposed on `GET
  /api/v1/users` and `/api/v1/auth/me`; `Subject` is `json:"-"`.
- Password sign-in: for a user whose provider is not `local`, `Login` compares
  against the dummy hash and returns `ErrInvalidCredentials`, so the timing equals
  that of an unknown name. `ChangePassword` and `ConfirmPassword` return
  `ErrNoPassword` (400): the recovery kit is for local admins only.

## Flow

- `GET /api/v1/auth/methods` is public and returns `{local, oidc: {enabled,
  display_name, available}}` (`available` is false in the desktop app).
- The browser routes live outside `/api/`, with handlers in
  `internal/server/oidc.go` and the rules in `auth.Service.LoginOIDC`.
- **`GET /auth/oidc/start?return_to=`**
  1. 404 when single sign-on is off or in the desktop build (the routes are not
     registered there). Throttled per client address (30 a minute).
  2. Generates state, nonce and a PKCE verifier (32 random bytes each).
  3. Sets `mr_oidc`: a secretbox seal (subkey `DeriveSubkey(master,
     "auth/oidc-flow")`, binding `At("cookie", "mr_oidc", "flow")`) of `{state,
     nonce, verifier, return_to, iat, issuer}`. Attributes: `HttpOnly`,
     `Path=/auth/oidc/`, `Max-Age=600`, `Secure` per `secureRequest`,
     **`SameSite=Lax`** (Strict would not be sent on the provider's cross-site
     redirect back).
  4. Redirects (302) to the authorization endpoint: code flow, S256, nonce, query
     response mode.
- **`GET /auth/oidc/callback?code&state`**
  1. Opens and clears the cookie. Refuses a missing cookie, one older than 10
     minutes, one for another issuer, a state mismatch (constant-time compare) and a
     state already in the in-memory used-state set. A provider `error` maps to
     `idp_error`; it is logged truncated through `logsafe` and never shown.
  2. Exchanges the code with the verifier (10 s timeout). The response must carry an
     `id_token`; access and refresh tokens are dropped.
  3. Verifies with go-oidc (signature, `iss`, `aud`) and additionally: `azp ==
     client_id` when `aud` has several values or `azp` is present; the nonce; `iat`
     at most 10 minutes old and not in the future; `nbf`; 60 s of leeway on `exp`,
     `nbf` and `iat`, against the auth service's clock; a non-empty `sub`.
  4. `LoginOIDC` applies the domain filter, then the mapping, and denies when the
     role is empty (`no_role`).
  5. In one transaction, `SignInExternalUser` finds or creates the user by subject
     (respecting `auto_create_users`) and updates the role, `updated_at` and
     `last_login_at`.
  6. Starts the session, then redirects to `return_to`.
- `return_to` must be a path: it starts with `/`, not `//` or `/\`, has no scheme or
  host after parsing, no control characters or backslashes, no percent-encoded
  slashes, backslashes or line breaks, and does not point into `/auth/`. Anything
  else becomes `/`. It is checked when the flow starts and again before the
  redirect.
- Errors redirect to `/?oidc_error=<code>`: `state_mismatch`, `idp_error`,
  `token_invalid`, `domain_not_allowed`, `no_role`, `account_conflict`, `disabled`,
  `throttled`. Tokens, codes, the verifier and raw claims never appear in responses,
  logs or the audit log.

## Roles, last admin, break-glass

- **Role recompute:** the role is recomputed at every OIDC sign-in. A change follows
  `UpdateUserRole` (sessions revoked, last-admin check) inside `SignInExternalUser`,
  and the audit log records `role_from`/`role_to`.
- **Last local admin:** while single sign-on is enabled, `DeleteUser` and
  `SetUserRole` refuse to remove the last local admin (`ErrLastLocalAdmin`, 409), in
  the same transaction as the change.
- **Residual `ErrLastAdmin`:** should a demotion through the provider hit the last
  admin anyway (no local admin left), the stored role is kept, the settings warning
  `oidc_role_kept` is raised, and the sign-in proceeds.
- **Manual role edits:** the role select is disabled for OIDC users while mappings
  exist ("managed by your identity provider"), and the server refuses the change
  with `ErrRoleManagedByProvider` (409).
- **admins_only:** a local non-admin gets `ErrLocalLoginDisabled` (403), but only
  after the password matched, so it reveals nothing to someone without it.

## Logout and lifetime

- Logout is unchanged. When `rp_logout` is on and the user is an OIDC user, the
  response adds `end_session_url` (`client_id` and `post_logout_redirect_uri`, the
  origin of the configured redirect URL plus `/`); the dashboard navigates there.
- Sessions follow `security.session_*`. There are no refresh tokens and no
  back-channel logout, so a user disabled at the provider keeps a running session
  until it ends: use a shorter absolute timeout with single sign-on. API keys an
  OIDC user created keep working until the user is deleted in MongoRescue.

## Audit

- **Callback:** recorded as `GET /auth/oidc/callback` with `provider=oidc`. On
  success the actor is the user, with `created`, `role_from` and `role_to`. A
  failure is anonymous with `reason=<code>`; the actor name is set only after the
  signature verified (`account_conflict`, `no_role`, `domain_not_allowed`).
  Refusals with different reasons are not coalesced together, and a coalesced
  summary keeps the reason.
- **admins_only refusals:** recorded as denied password sign-ins with
  `reason=local_login_disabled`.
- **`/auth/oidc/start`:** throttled, not audited.

## Desktop

The desktop app has no single sign-on: the routes are not registered,
`/api/v1/auth/methods` reports it unavailable, the policy is always off, turning it
on is refused, and Settings → Single sign-on explains why.

## Tests

- **Fake provider:** `internal/auth/oidc/oidctest`, an httptest provider with
  generated RSA and ECDSA keys serving discovery, JWKS, authorize and token, with
  hooks for claims, the signature (`alg`, `kid`, `none`, HS256, mismatched key type,
  an unpublished key), key rotation, one-time codes, PKCE and errors. It is the pull
  request gate.
- **Security cases** (`internal/auth/oidc`, `internal/auth`, `internal/server`):
  forged, missing and expired state; a cookie sealed with another key or binding or
  without a verifier; a replayed code or state; wrong `iss` or `aud`; several `aud`
  without `azp`; a wrong `azp`; `alg=none`; HS256 signed with the client secret; an
  `alg`/`kty` mismatch; rotation, where an unknown `kid` is refetched once; an
  expired token; a future or stale `iat`; `nbf`; a wrong or missing nonce; a missing
  or wrong verifier; domain filter bypasses (`x@corp.com.evil.com`,
  `x@evilcorp.com`, `X@CORP.COM` (allowed: domains are case-insensitive),
  `email_verified` false, the string `"true"`, missing); `return_to` `//evil`,
  `/\evil`, `https://evil`, `/%2f%2fevil`, CR/LF; the last local admin and
  break-glass; a username collision; one dummy-hash comparison for OIDC users on the
  password form; and no tokens, codes or claims in logs, the audit log or responses.
- **Keycloak:** `internal/integration` (`integration` tag) signs in through a real
  Keycloak in dev mode with an imported realm, driving its login form, when
  `MONGORESCUE_TEST_KEYCLOAK_URL` is set. `scripts/test-integration-docker.sh`
  starts Keycloak; CI runs it in the separate, non-required job
  "Integration (Keycloak SSO)" on pushes to main and on the nightly schedule.

## Compatibility

Nothing changes while single sign-on is off. The migration only adds columns; a
release before v0.18.0 refuses a database at schema 20, so restore a metadata
snapshot taken before the upgrade to go back.
