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

`payment-api` is the I03 runtime reference composition root. It serves public
`/health/live` and OIDC-backed `/health/ready` endpoints plus protected customer,
operations and integration demonstration routes; no payment business endpoint is
implemented yet. `core-banking-service` (B01) serves the authenticated Core Banking
port; see ADR 0008. The other seven executables remain feature-phase placeholders.
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
