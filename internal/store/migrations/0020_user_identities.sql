-- 0020_user_identities: users signed in through an OpenID Connect provider.
--
-- Every user gains auth_provider: local (a password stored as a bcrypt hash) or
-- oidc (single sign-on). Users stored before this migration are local. An OIDC
-- user is identified only by subject, the provider's issuer and its subject
-- identifier joined by '#', never by username or email, and stores an empty
-- password_hash, so the password form can never sign them in. The store keeps
-- auth_provider, subject and password_hash consistent: oidc if and only if a
-- subject is set if and only if the hash is empty. Releases before this one
-- refuse a database at this version.

ALTER TABLE users ADD COLUMN auth_provider TEXT NOT NULL DEFAULT 'local' CHECK (auth_provider IN ('local','oidc'));
ALTER TABLE users ADD COLUMN subject TEXT;  -- iss + '#' + sub
CREATE UNIQUE INDEX users_by_subject ON users (subject) WHERE subject IS NOT NULL;
