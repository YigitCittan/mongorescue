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
- Every breaking change is listed under `### Changed` in `CHANGELOG.md`, prefixed with **Breaking**, with what to do about it.
- From 1.0, a major release is a mandatory update in the desktop app (see [desktop.md](desktop.md#updates)); minor and patch releases are optional.

## When to release

- Work is released in batches: a release is cut when a planned set of changes is merged and green, not after every pull request.
- Security fixes and fixes for data-loss or failed-backup bugs are released as soon as they are merged, as a patch release.
- A version is never reused. If a tag was pushed but its release failed, the fix ships under the next version and the changelog says so.

## What 1.0 means

`1.0.0` is the first release whose REST and MCP APIs, configuration and storage formats are kept stable within the major version. It is cut once:

- backups are verified after upload and restorable backups are proven by scheduled restore tests;
- running backups and restores can be cancelled, and every run keeps its log;
- the desktop app has updated itself in place on Windows, macOS and Linux in real use;
- the API has had one minor release without breaking changes.

## How to cut a release

See [CONTRIBUTING.md](../CONTRIBUTING.md#versioning-and-releases).
