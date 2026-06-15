# packages/protocol

[`openapi.yaml`](openapi.yaml) is the **source of truth** for the UFO
control-plane HTTP API — the contract shared by the Go server (`apps/api`), the
web client (`apps/web`), and the Rust rover (`apps/rover`).

This is a preview protocol with no compatibility guarantee. Endpoints, payloads,
authentication behavior, and rover interactions may change without notice.

## Status

The spec is **maintained by hand alongside the code** — when you change an
endpoint, update [`openapi.yaml`](openapi.yaml) in the same change. Generated
clients (TS for web, Rust for the rover, Go types for the server) are a planned
follow-up.

## Validate / preview

```bash
# lint the spec
npx --yes @redocly/cli@2.32.2 lint packages/protocol/openapi.yaml

# preview docs locally
npx --yes @redocly/cli@2.32.2 preview-docs packages/protocol/openapi.yaml
```

## Planned codegen (follow-up)

```bash
# TypeScript types for the web client
npx openapi-typescript packages/protocol/openapi.yaml -o apps/web/lib/api-types.ts

# Rust client for the rover (e.g. via progenitor or openapi-generator)
# Go server types (e.g. via oapi-codegen)
```
