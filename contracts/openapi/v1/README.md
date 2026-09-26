# Synchronous API contracts, version 1

`shared.json` is an OpenAPI 3.1.1 reusable component library with shared money/ID/
error schemas, correlation and idempotency headers, bounded pagination and a
bearer security scheme. Its paths stay empty. Feature documents reference its components: `core-banking.json`
(B01) specifies the internal Core Banking port, and handler tests validate
responses against it. `payment.json` (B02) specifies customer payment commands,
receipts and command reads; the payment service bundles it with
`contracts.BundleOpenAPI`, generates its `net/http` boundary with oapi-codegen and
validates requests with kin-openapi. ADR 0002 specifies API semantics; I03 and feature phases
implement authentication, scopes and operations. Tests validate the official
OpenAPI document structure and compile referenced schemas offline.
