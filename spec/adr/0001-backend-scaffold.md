# ADR 0001: Backend module and package scaffold

Date: 2026-09-26. Status: accepted for I01 scaffolding.

Follow-up: [ADR 0002](0002-shared-contracts-and-api-conventions.md) completes the
shared contracts; [ADR 0003](0003-toolchain-and-checks.md) supersedes the initial
Go version and test-dependency deferral with Homebrew Go 1.27.1 and baseline checks.

## Context

Foundation Sections 2, 4 and 12 prescribe Go, one module, nine separate binaries,
independent financial authorities and transport-independent domain rules. This
repository initially contains only manuscript and planning documents.

## Decision

Use `backend/go.mod` with the local import namespace `gtb.local/backend` and
Go 1.26.8, a supported release listed in the
[official Go downloads metadata](https://go.dev/dl/?mode=json) on 2026-09-26.
The exact patch version is recorded in the module's `go` directive. This sets
the minimum toolchain; automated checks will need to select that exact version
when baseline CI is added. No third-party dependencies are needed for this item.
Temporal, database and transport libraries remain unselected until their initial
compatibility decisions are recorded.

Keep the nine foundation entry points in `backend/cmd/`. Until service runtime
is implemented, each prints an explicit unimplemented message to stderr and
exits nonzero. A scaffold must not appear to be a healthy running service.

Reserve package boundaries with Go package documentation. The payment domain is
pure: no HTTP, Temporal, storage or outer-layer dependencies. Application
operations depend on domain rules and define narrow ports; storage and adapters
implement those ports. Composition roots wire implementations. Core Banking,
compliance and AML retain distinct ownership; shared contracts do not grant
access to another service's database.

Reserve `contracts/openapi/v1`, `contracts/events/v1` and
`contracts/simulators/v1` for the synchronous and asynchronous contracts in
foundation Section 8. Their READMEs explicitly identify the absence of schemas.

## Consequences and follow-up

The scaffold can compile without external modules or infrastructure. Package
documentation establishes intended ownership but does not yet enforce all
future dependency edges. The first domain dependency inspection checks the
current empty dependency set.

Money and identifier types, API/error/event conventions, contract tests,
dependency pins and baseline CI remain unchecked I01 work. This ADR does not
decide business or transport contracts. No foundation scope has changed.
