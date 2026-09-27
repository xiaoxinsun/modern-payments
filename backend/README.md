# GTB backend scaffold

One Go module builds the nine independent service binaries listed in foundation
Section 12. The module path `gtb.local/backend` is a repository-local import
namespace; it does not require a published remote module.

Use Go **1.27.1** on PATH. The module and CI pin that version; automatic toolchain
downloads are disabled. Runtime code uses focused pinned dependencies for
JWT/JWKS validation and UUID mechanics; JSON Schema validation is a pinned test
dependency. See [ADR 0003](../spec/adr/0003-toolchain-and-checks.md) for toolchain
details and [ADR 0007](../spec/adr/0007-maintained-libraries-and-project-owned-code.md)
for the dependency review and future adoption register.

From the repository root:

```sh
make deps   # Download pinned dependencies once; requires network.
make check  # Toolchain, formatting, modules, vet, tests, race detector, builds.
```

Individual targets: `make fmt` (rewrite formatting), `make fmt-check`, `make vet`,
`make test`, `make race`, `make mod-check`, and `make build`. Build artifacts go to
ignored `.build/bin/`. The race detector requires a C compiler (Xcode Command
Line Tools on macOS or the compiler supplied by the Ubuntu CI image).

Tests cover money bounds and exact serialization, UUID identities, JSON Schema
and OpenAPI validation, event/error envelopes, and transitive domain dependencies.
The versioned fixtures and vendored OpenAPI meta-schema make contract validation
offline after `make deps`. Integration tests arrive with infrastructure/features.

`payment-api` serves the B02 payment command API (durable receipts, command and
receipt reads; ADR 0009) beside the I03 runtime demonstration routes.
`core-banking-service` (B01) serves the authenticated Core Banking port and
relays its posting outbox to Kafka; see ADR 0008. B03 (ADR 0010) adds
`payment-worker` (payment outbox relay), `event-projector` (payment projections,
gap catch-up and `rebuild`) and `query-api` (payment queries). Generated code is
checked in: `make sqlc-generate` for queries and `make openapi-generate` for the
payment and query HTTP boundaries (tests fail when an embedded contract bundle
is stale). The other four executables remain feature-phase placeholders.
See the local runbook for `make auth-smoke` and runtime environment variables.
For administrator/owner connection details, database and schema inventory, and
interactive `docker exec` behavior, see the [database guide](migrations/README.md).

## Ownership and dependency direction

- `cmd/` contains process composition roots. Keep business rules out of them.
- `internal/payment/domain` owns pure payment rules and state. It must not import
  HTTP, Temporal, persistence, application, workflow or adapter packages.
- `internal/payment/application` coordinates domain operations and owns the
  narrow interfaces its callers and infrastructure implementations need.
- `internal/payment/storage` implements payment persistence.
- `internal/payment/api` is the generated/validated HTTP boundary; it maps wire
  types to domain types and never decides authorization or product rules.
- `internal/workflows` coordinates durable orchestration through application
  operations; `internal/adapters` implements external ports.
- `internal/corebanking`, `internal/compliance`, and `internal/amlmonitoring`
  retain their own authority. Their future pure domain subpackages follow the
  same transport-independent rule as the payment domain.
- `internal/projections` owns query models; `internal/reconciliation` compares
  independent evidence; `internal/simulators` owns synthetic external records.

`internal/platform/value` provides pure money and identity types.
`internal/contracts` provides event/error envelopes; it is an outer boundary
package, not a domain dependency. Other packages currently contain documentation
only. I02 supplies database ownership/migrations and I03 supplies the authenticated
service runtime; business persistence arrives with feature phases. Shared wire
contracts live in [contracts](../contracts/README.md);
sharing a schema never authorizes cross-service database access.
