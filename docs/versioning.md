# Versioning and release policy

MongoRescue follows [Semantic Versioning 2.0](https://semver.org). The version number tells you what an upgrade can do to your setup, so it is chosen by rule, not by feel.

## Which number changes

Classify every change in a release. The release takes the highest bump any of its changes needs.

| Change | Examples | Before 1.0 (`0.y.z`) | From 1.0 (`x.y.z`) |
| :--- | :--- | :--- | :--- |
| **Breaking** | A REST/MCP field, endpoint, flag or environment variable is removed, renamed or changes meaning; requests that used to succeed are now refused; old backups, configuration or metadata databases can no longer be read; a migration cannot be undone by restoring the previous `mongorescue.db`; a platform or MongoDB version is no longer supported | **minor** (`0.8.x` → `0.9.0`) | **major** (`1.4.2` → `2.0.0`) |
| **Feature** | A new endpoint, field, setting, dashboard feature, MCP tool or notification channel; a new metadata migration that older data upgrades through | **minor** | **minor** (`1.4.2` → `1.5.0`) |
| **Fix** | A bug or security fix, documentation, tests, dependency updates without behaviour changes. No new features, no migrations | **patch** (`0.9.0` → `0.9.1`) | **patch** (`1.4.2` → `1.4.3`) |

Rules that follow from the table:

- A patch release never contains a migration or a new feature, so it is always safe to install and to roll back from.
- A released migration is never edited: since v0.14.0 a binary whose embedded migration differs from the one applied to the database refuses to start (`ErrMigrationChanged`); a downgrade is still refused with `ErrSchemaTooNew`. Since the start that applies pending migrations copies the database first (`mongorescue.db.pre-v<from>-to-v<to>`), a feature release can be undone by restoring that copy; see [upgrading.md](upgrading.md#rolling-back).
- Every breaking change is listed under `### Changed` in `CHANGELOG.md`, prefixed with **Breaking**, with what to do about it, and again under `### Upgrade notes`, which the release page repeats at its top.
- The one-time imports of deprecated environment variables, `config.json` and `state.json` are removed before 1.0.0, because removing them afterwards would need a major release ([configuration.md](configuration.md#upgrading-deprecated-environment-variables-and-configjson)).
- From 1.0, a major release is a mandatory update in the desktop app (see [desktop.md](desktop.md#updates)); minor and patch releases are optional.

## When to release

- Work is released in batches: a release is cut when a planned set of changes is merged and green, not after every pull request.
- Security fixes and fixes for data-loss or failed-backup bugs are released as soon as they are merged, as a patch release.
- A version is never reused. If a tag was pushed but its release failed, the fix ships under the next version and the changelog says so.

## What 1.0 means

`1.0.0` is the first release whose REST and MCP APIs, configuration and storage formats are kept stable within the major version. It is cut once:

- backups are verified after upload and restorable backups are proven by scheduled restore tests;
- running backups and restores can be cancelled, and every run keeps its log;
- the desktop app's updates have worked in real use: in place on Windows; on macOS and Linux the app downloads, verifies and reveals the new build (see [desktop.md](desktop.md#updates)), and an automated update test passes on all three;
- the macOS app is signed with an Apple Developer ID and notarized, so Gatekeeper opens it without a warning;
- the API has had one minor release without breaking changes;
- phases 1 and 2 of the [roadmap](roadmap.md) are done: recovery you can prove (self-backup, recovery kit, users and roles, restore preflight and verification, RPO/RTO, a full audit log) and roles with single sign-on.

## Helm chart versions

The Helm chart (`deploy/helm/mongorescue`) is published as a signed OCI artifact, `oci://ghcr.io/yigitcittan/charts/mongorescue`, and has its own semantic version, independent of the application's: `version` in `Chart.yaml`. `appVersion` is not edited by hand; the release workflow sets it to the tag at package time (`helm package --app-version`), and the default `image.tag` follows it.

| Change to the chart | Bump |
| :--- | :--- |
| A value is removed or renamed, its default or meaning changes, or the new rendering needs action on upgrade (for example a changed selector or a new required value) | **major** (before 1.0 of the chart: minor) |
| A new value, template or resource, behind a default that keeps the old rendering | **minor** |
| A fix, a label or documentation fix inside the chart, a dependency of the render | **patch** |

CI (`scripts/check-chart-version.sh`, the *Helm chart version* job of `helm.yml`) fails a pull request that changes any file of the chart without raising `version` relative to the base branch. Changes under `deploy/helm/mongorescue/ci/` (test values) and Markdown files are exempt. A published chart version is immutable: if a release finds its chart version already in the registry (a release without chart changes), the workflow keeps the existing chart and does not push again, so the chart version tells what the templates and values are, while `image.tag` selects the application.

## How to cut a release

See [CONTRIBUTING.md](../CONTRIBUTING.md#versioning-and-releases). Its first step reviews the README's [known limitations](../README.md#known-limitations) and the [production checklist](production.md#production-checklist), so they match what the release ships.
