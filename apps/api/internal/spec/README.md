# OpenAPI spec

[`openapi.yaml`](openapi.yaml) is the **source of truth** for the UFO Hub HTTP
API — the contract shared by the Go server (`apps/api`), the web client
(`apps/web`), and the Rust CLI (`apps/rover`). It is **embedded** into the Hub
binary (`spec.go`) and served at `/openapi.yaml`; the RFC 9727 catalog at
`/.well-known/api-catalog` points to it.

This is a public beta protocol. Endpoints, payloads, authentication behavior,
and rover interactions may still change before 1.0; tagged releases should be
matched with their documented API version and release notes.

## Status

The spec is **maintained by hand alongside the code** — when you change an
endpoint, update [`openapi.yaml`](openapi.yaml) in the same change. Generated
clients (TS for web, Rust for the rover, Go types for the server) are a
planned follow-up.

## Validate / preview

```bash
# lint the spec
npx --yes @redocly/cli@2.36.0 lint apps/api/internal/spec/openapi.yaml

# preview docs locally
npx --yes @redocly/cli@2.36.0 preview-docs apps/api/internal/spec/openapi.yaml
```

## Planned codegen (follow-up)

```bash
# TypeScript types for the web client
npx openapi-typescript apps/api/internal/spec/openapi.yaml -o apps/web/lib/api-types.ts

# Rust client for the rover (e.g. via progenitor or openapi-generator)
# Go server types (e.g. via oapi-codegen)
```
