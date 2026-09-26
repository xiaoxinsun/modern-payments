# ADR 0007: Maintained libraries and project-owned code

Date: 2026-09-26. Status: accepted for the implementation plan and I03
retrofit. Supersedes ADR 0003's statement that runtime code uses only the
standard library and ADR 0006's standard-library JWT implementation decision.

## Context and selection rule

The first I03 implementation hand-built compact-JWT parsing, RSA signature
verification, JWK conversion and a refresh cache. Those are security-sensitive
protocol mechanics with maintained implementations and should not be local
infrastructure code. A review then covered both existing code and planned
boundaries so the same mistake is not repeated in later phases.

Adopt a library when it implements a protocol, cryptographic format, durable
client, database driver, telemetry standard or mechanical schema generation.
Before pinning it, the owning phase must check active maintenance, a stable
release, compatible Go/Node versions, license, vulnerability status, transitive
surface, cancellation/timeouts and testability. Pin exact direct versions and
commit lock/checksum files. Run normal tests plus `govulncheck`/package audit once
the corresponding ecosystem is present. Do not add a library merely to wrap a
small standard-library API or to hide a business decision.

GTB retains code whose behavior is the product: money and posting invariants,
idempotency scope, authorization, maker/checker rules, workflow transitions,
effect ownership, reconciliation and unknown-outcome handling. A dependency may
provide mechanics underneath these rules but cannot decide them.

## Retrospective decisions

- Keep `github.com/santhosh-tekuri/jsonschema/v6 v6.0.2` for draft 2020-12
  contract validation.
- Replace hand-written UUID generation/parsing with
  `github.com/gofrs/uuid/v5 v5.5.1`. Keep the `value.ID` wrapper because canonical
  UUIDv4-only encoding and the rule that identifiers grant no authority are GTB
  boundary semantics.
- Replace the custom JWT/JWKS implementation with
  `github.com/golang-jwt/jwt/v5 v5.3.1` and
  `github.com/MicahParks/keyfunc/v3 v3.8.2`. The former validates JOSE/JWT
  signatures and registered claims; the latter fetches, caches and refreshes
  remote JWKS, including unknown-key rotation. GTB still restricts algorithms,
  issuer, audience and expiry, bounds token size, maps Keycloak claims to a
  minimal principal, ignores roles belonging to other audiences, and enforces
  endpoint/customer policy. The selected current modules are permissively
  licensed (MIT/Apache-2.0; transitive Go modules use compatible permissive
  licenses), and the official Go vulnerability scan reported no findings.
- Keep the I02 migration wrapper. `goose` was considered, but ordinary per-file
  version tracking and transactions do not replace I02's required immutable
  SHA-256 history verification and one transaction covering validation plus all
  pending files for a domain. The wrapper contains no SQL parser or database
  driver; it orchestrates the pinned PostgreSQL `psql` client and is covered by
  checksum, concurrency and rollback integration tests.
- Keep the shared runtime on `net/http`, `log/slog`, `context`, `os/signal` and
  narrow middleware. Correlation/error envelopes, secret redaction, readiness,
  shutdown order, role/scope checks and development-control isolation are local
  application policy. A web framework or configuration framework would add a
  second abstraction without replacing those decisions.

## Planned dependency ownership

The tracker dependency register is authoritative for phase ownership. The
choices are:

- B01: `jackc/pgx/v5` for PostgreSQL and `sqlc` for generated queries. Add the
  OpenTelemetry Go API/SDK and maintained `otelhttp` plus datastore/client
  instrumentation as each transport is introduced.
- B02: `oapi-codegen/v2` using its standard-library HTTP server output and
  `kin-openapi/openapi3filter` request validation at the boundary. Generated
  models/handlers are boundary code; domain types and authorization remain separate.
- B03: `twmb/franz-go` for Kafka. GTB retains the transactional outbox,
  deduplication/checkpoint, gap and replay policies.
- B04: the official `go.temporal.io/sdk` and `testsuite`. GTB workflow code must
  obey determinism and own its state machines; later phases reuse the same SDK.
- B07: `sony/gobreaker/v2` for eligible adapter circuit state. Temporal retry
  policy and explicit adapter rules remain authoritative; no generic retry may
  resubmit a possibly-effectful financial operation after an ambiguous result.
- B13: Go `encoding/csv`; importing rows safely is a domain/application concern,
  not a reason for another parser dependency.
- F01: React, TypeScript and Vite; `react-oidc-context`/`oidc-client-ts` for the
  browser authorization-code-with-PKCE session; TanStack Query for server state;
  `@hey-api/openapi-ts` for the generated Fetch client/query bindings; Playwright
  and `@axe-core/playwright` for browser and automated accessibility checks.
- V01: Grafana k6 for load profiles and thresholds, plus the Temporal SDK replay
  facilities for workflow history. Restore/reconcile/effect-suppression remains
  a GTB-specific procedure.

Exact future versions are intentionally not frozen years of phases in advance.
Each owner pins and records the current compatible release when implementation
begins. Switching a named dependency requires an ADR amendment with migration
impact, rather than an ad hoc addition in feature code.

## Consequences

The security-critical JWT implementation is smaller and delegates protocol
correctness and key rotation to maintained packages while preserving explicit
banking authorization rules. UUID behavior is also library-backed without
changing its contract. Direct and transitive modules now appear in `go.mod` and
`go.sum`; normal module, race and live OIDC checks cover their integration.

Libraries reduce commodity code, not accountability. Generated code is checked
for drift, dependency boundaries stay narrow behind project interfaces, and
feature tests must still prove GTB invariants under duplication, concurrency,
timeouts, restart and authorization failures.
