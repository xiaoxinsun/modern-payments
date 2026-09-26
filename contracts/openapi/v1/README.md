# Synchronous API contracts, version 1

`shared.json` is an OpenAPI 3.1.1 reusable component library with shared money/ID/
error schemas, correlation and idempotency headers, bounded pagination and a
bearer security scheme. Its paths stay empty. Feature documents reference its components: `core-banking.json`
(B01) specifies the internal Core Banking port, and handler tests validate
responses against it. ADR 0002 specifies API semantics; I03 and feature phases
implement authentication, scopes and operations. Tests validate the official
OpenAPI document structure and compile referenced schemas offline.
