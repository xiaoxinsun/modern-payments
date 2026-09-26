# Synchronous API contracts, version 1

`shared.json` is an OpenAPI 3.1.1 reusable component library with shared money/ID/
error schemas, correlation and idempotency headers, bounded pagination and a
bearer security scheme. Paths are empty; no endpoint or authentication enforcement
is implemented in I01. ADR 0002 specifies API semantics; I03 and feature phases
implement authentication, scopes and operations. Tests validate the official
OpenAPI document structure and compile referenced schemas offline.
