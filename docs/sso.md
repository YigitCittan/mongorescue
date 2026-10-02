# Single sign-on

MongoRescue can let people sign in through an OpenID Connect (OIDC) identity
provider such as Microsoft Entra ID, Google, Okta or Keycloak. Groups in the
provider's ID token decide each person's [dashboard role](design/roles.md). Local
accounts keep working, and a local administrator is always the way in when the
provider is down. API keys, the [command line](cli.md) and [MCP](mcp.md) are
unchanged. Single sign-on is not available in the [desktop app](desktop.md).

How it works and why is in [design/oidc.md](design/oidc.md); every setting is
listed in [configuration.md](configuration.md#single-sign-on).

## Before you start

- Serve MongoRescue over HTTPS behind a reverse proxy ([production.md](production.md#tls-and-the-reverse-proxy)),
  with *Trust proxy headers* on or *Secure cookies* set to *Always*.
- Keep at least one **local administrator** with a strong password. MongoRescue
  refuses to turn single sign-on on without one, and refuses to delete or demote
  the last one while single sign-on is on.
- The **callback URL** is `https://<your MongoRescue host>/auth/oidc/callback`.
  Settings → Single sign-on pre-fills it from the address you use; copy it into the
  provider exactly as shown. MongoRescue never derives it from the request.

## Setting it up

1. Register a client (application) at your provider with the callback URL as its
   redirect URI and the authorization code flow. A confidential client (with a
   secret) is the usual choice; a public client works too, protected by PKCE.
2. In MongoRescue, open **Settings → Single sign-on** and fill in the issuer URL,
   the client ID and secret. Click **Test provider**: it fetches the discovery
   document and keys and shows the endpoints and signature algorithms
   (RS256 or ES256 are required).
3. Add **group mappings**: each maps a value of the groups claim to `viewer`,
   `operator` or `admin`; a member of several groups gets the highest role. Without
   a matching group a person gets the **role without a matching group**: none (the
   sign-in is refused), `viewer` or `operator`. The admin role only ever comes from
   a mapping.
4. Optionally limit sign-in to **allowed email domains** (the provider must report
   the email as verified) and turn off **Create users at their first sign-in** so
   that only people who already signed in once may sign in again.
5. Turn **Enable single sign-on** on and save. The sign-in page now shows the
   button. Choose *Password sign-in: local administrators only* to send everyone
   else through the provider; it ends the sessions of other local users.

The first sign-in creates a user named after the `preferred_username` claim (or the
email, or the subject). People are recognised by the provider's issuer and subject
only: an existing local user with the same name is **never** taken over. Such a
sign-in fails with *A user with your name already exists*; rename one of the two
users. Roles are recomputed at every sign-in, and a role change ends the user's
sessions.

## Provider recipes

### Microsoft Entra ID

1. *Entra admin center → App registrations → New registration*. Supported account
   types: *Accounts in this organizational directory only*. Redirect URI: *Web*,
   the callback URL.
2. *Certificates & secrets → New client secret*; copy the value into **Client
   secret**. The **Client ID** is the *Application (client) ID*.
3. *Token configuration → Add groups claim → Security groups*, ID token: *Group ID*.
   Group mappings then use **group object IDs** (GUIDs such as
   `1f2e3d4c-…`), not names.
4. **Issuer**: `https://login.microsoftonline.com/<tenant-id>/v2.0` with your
   *Directory (tenant) ID*. The multi-tenant `/common` and `/organizations`
   endpoints are refused: they are not issuers and would accept other tenants.
5. Groups claim: `groups`. Username claim: `preferred_username` (the UPN).

Entra ID puts at most 200 groups in a token. A user in more groups gets an
"overage" reference instead of the list, which MongoRescue treats as **no groups**:
such users get the role without a matching group. Assign the application to the
groups that matter (*Enterprise applications → Users and groups*) and choose *Groups
assigned to the application* in the groups claim to stay below the limit.

### Google

Google Workspace has no groups claim in its ID tokens, so use the domain filter and
a default role:

1. *Google Cloud console → APIs & Services → Credentials → Create credentials → OAuth
   client ID*, type *Web application*, with the callback URL as an authorised
   redirect URI. Configure the consent screen as *Internal* for Workspace.
2. **Issuer**: `https://accounts.google.com`. Client ID and secret from the
   credential.
3. **Allowed email domains**: your Workspace domain, such as `example.com`.
4. **Role without a matching group**: `viewer` or `operator`. Every Google user
   gets this role at every sign-in (a role set by hand is replaced at the next
   sign-in), so keep administration to local accounts.
5. Username claim: `email` (Google sends no `preferred_username`).

### Okta

1. *Applications → Create App Integration → OIDC - OpenID Connect → Web
   Application*. Sign-in redirect URI: the callback URL. Grant type: authorization
   code.
2. *Sign On → OpenID Connect ID Token → Groups claim*: type *Filter*, name `groups`,
   for example *Matches regex* `backup-.*`. (With a custom authorization server, add
   a `groups` claim to the ID token there instead.)
3. **Issuer**: `https://<your-org>.okta.com` for the org authorization server, or
   `https://<your-org>.okta.com/oauth2/default` for the default custom one. Use
   exactly the issuer shown in the discovery document.
4. Groups claim: `groups`; map Okta group names such as `backup-admins` to roles.
   Add the `groups` scope under **Scopes** when your authorization server requires
   it.

### Keycloak

1. In your realm, *Clients → Create client*, type *OpenID Connect*, client
   authentication on, standard flow on. Valid redirect URIs: the callback URL.
   *Credentials* holds the client secret.
2. Group membership: *Client scopes → <client>-dedicated → Add mapper → By
   configuration → Group Membership*, token claim name `groups`, **Full group path
   off** (otherwise the values are `/backup-admins`), *Add to ID token* on.
3. **Issuer**: `https://<keycloak host>/realms/<realm>`.
4. Groups claim: `groups`. To map realm roles instead, use the claim path
   `realm_access.roles` (and *Add to ID token* on the *roles* client scope's realm
   roles mapper).

## Signing out

Signing out ends the MongoRescue session. With **Also sign out at the identity
provider** on, MongoRescue then sends single sign-on users to the provider's
`end_session_endpoint`, so the provider session ends too and they come back to the
sign-in page.

MongoRescue sessions do not follow the provider's: there are no refresh tokens and
no back-channel logout. A person disabled at the provider keeps a running session
until it expires, so shorten **Settings → Security → Maximum session length** (for
example to `12h`) when you use single sign-on, and delete users in MongoRescue when
people leave: their API keys keep working until then.

## Troubleshooting

A failed sign-in returns to the sign-in page with a message; the audit log
(Settings → Audit log, action `GET /auth/oidc/callback`) records the reason.

| Reason | Meaning |
| :--- | :--- |
| `state_mismatch` | The sign-in took longer than 10 minutes, was started in another browser or tab, or cookies are blocked. Start again |
| `idp_error` | The provider refused (for example the user cancelled, or the client secret or redirect URI is wrong) or was unreachable. MongoRescue's log has the provider's error code |
| `token_invalid` | The ID token failed verification: wrong client ID, an issuer that differs from the configured one, a signature algorithm other than RS256/ES256, or a clock more than a minute off |
| `domain_not_allowed` | The email is not verified or its domain is not in the allowed list |
| `no_role` | No group mapping matched and there is no default role, or the person has no user and automatic creation is off |
| `account_conflict` | A user with the same name exists; rename one of them |
| `disabled` | Single sign-on was turned off |
| `throttled` | More than 30 sign-in requests a minute from the same address |

If an identity provider sign-in would have demoted the last administrator, the
stored role is kept and Settings → Single sign-on shows a warning: map a group to
admin or keep a second administrator.
