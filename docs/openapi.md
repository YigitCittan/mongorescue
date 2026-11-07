# OpenAPI Description

The REST API under `/api/v1` is described by an [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0) document: [`openapi.yaml`](openapi.yaml). It is the machine-readable contract of the API; [REST API Reference](api.md) is the prose reference with the field semantics, examples and edge cases.

## Getting the document

- In the repository: [`docs/openapi.yaml`](https://github.com/YigitCittan/mongorescue/blob/main/docs/openapi.yaml), the source (hand-written).
- From a running instance, with any session or API key (read scope): `GET /api/v1/openapi.json`, the same document as JSON, always the one of the running version.

```bash
curl -s -H "Authorization: Bearer $MONGORESCUE_API_KEY" http://localhost:8080/api/v1/openapi.json | jq '.paths | keys | length'
```

Feed either to a generator (`openapi-generator`, `oapi-codegen`, `openapi-typescript`), a request validator, Postman or Insomnia.

## What it contains

- Every `/api/v1` route with its method, path and query parameters, request body, and the responses it can answer. The dashboard, `/metrics`, the MCP endpoint (`/mcp`) and the single sign-on redirects (`/auth/oidc/*`) are not part of it; see [monitoring](metrics.md), [MCP](mcp.md) and [single sign-on](sso.md).
- The envelope `{"success", "data", "error", "code"}` that every response uses.
- The `code` of error responses as the closed enum of [error codes](api.md#error-codes). `GET /api/v1/ready` answers `503` with its own `ReadyCode` instead (`not_active`, `draining`, `store_unwritable`, `disk_full`).
- The security schemes: the `mr_session` cookie with the `X-CSRF-Token` header for unsafe methods, and API keys as `Authorization: Bearer` or `X-API-Key`. The public routes declare no security.
- `x-required-scope` on every authenticated operation: the lowest scope (`read`, `operator`, `admin`) the route table requires. Some routes then apply finer rules (own account, in-place restores, the two-person rule), described in [api.md](api.md#api-key-scopes).

Resource schemas (`Backup`, `Job`, `Connection`, ...) list the stable core fields and allow additional properties; clients must ignore fields they do not know. Routes whose payload is not modelled yet answer the plain envelope with a free-form `data`.

## How it is kept true

Unit tests fail when the document drifts from the server:

- the set of paths and methods equals the registered routes, exactly;
- every error code of `internal/apierr` is in the `ErrorCode` enum, and nothing else is;
- `x-required-scope` equals the scope of the route table, and every unsafe operation asks for the CSRF token;
- the document is structurally valid (references resolve, path parameters are declared, operation IDs are unique);
- the embedded JSON served at `/api/v1/openapi.json` equals `docs/openapi.yaml`. After editing the YAML, regenerate it with `go test ./internal/server -run OpenAPI -update`.

On every pull request CI compares `docs/openapi.yaml` with the one of the last release tag using [oasdiff](https://github.com/oasdiff/oasdiff). A breaking change (a removed route or field, a new required parameter, a narrowed type) fails the check unless the `[Unreleased]` section of [CHANGELOG.md](https://github.com/YigitCittan/mongorescue/blob/main/CHANGELOG.md) has a `**Breaking:**` entry for it. Run it locally with `scripts/check-openapi-breaking.sh` (needs Docker). What counts as breaking is defined in the [versioning policy](versioning.md).
