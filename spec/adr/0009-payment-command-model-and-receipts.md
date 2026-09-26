# ADR 0009: Payment command model and durable receipts

Date: 2026-09-26. Status: accepted for B02.

## Context

Foundation Section 6 requires one payment-database transaction that persists the
instruction, idempotency record, `PaymentReceived` event and workflow-start
dispatch, and a `202` that means "durably received", not accepted. Section 7
requires unique logical identities, optimistic aggregate versions, and each
transition to append evidence, audit, events and outbox rows atomically. Section 5
requires every transition to record evidence, actor, policy version and aggregate
version, and forbids treating an unknown outcome as failure. B02 must also prepare
the submission/cancellation boundary and execution ownership without starting
workflows (B06).

## Decision

- **Aggregate and history.** `payments` holds the current lifecycle, execution
  outcome, version, exclusive claim and execution-owner reference.
  `payment_instructions` stores the immutable instruction. `payment_versions`
  stores one append-only row per version: change kind, from/to lifecycle and
  outcome, actor, policy version, reason, evidence references and command ID.
  Database guards make instructions, history, idempotency records, audit entries
  and domain events append-only. A payment update must advance exactly one
  version, keep its identity, claim and owner, and have a matching history row
  (deferred constraint trigger).
- **Domain rules** (`internal/payment/domain`) define the lifecycle graph from
  foundation Section 5. A submission claim is required for `EXECUTING` and any
  execution outcome. `COMPLETED` needs `CONFIRMED_SUCCESS` plus evidence.
  Rejection is refused while the outcome is `PENDING`, `UNKNOWN` or
  `CONFIRMED_SUCCESS`. `UNKNOWN` moves only to a confirmed outcome, with evidence.
  Stale expected versions return `ErrVersionConflict`.
- **Claims and execution ownership.** One exclusive claim column arbitrates
  cancellation against submission: a submission claim needs `ACCEPTED`, and a
  cancellation claim needs a pre-execution state. Whichever claim commits first
  wins, and the other gets `ErrClaimConflict` (or a version conflict). Repeating
  the winning claim ID is a no-op replay. After a cancellation claim the only
  allowed transition is `CANCELLED`; after a submission claim the payment cannot
  return to pre-execution states. The execution owner must be the stable
  `execution/{paymentId}/{product}` ID and cannot be changed once set.
  `storage.Store.Apply` is the single versioned change primitive: it locks the
  row, runs the domain mutation, and then appends history, events, outbox and
  audit and applies a conditional `UPDATE … WHERE version = expected` in one
  transaction.
- **Receipt transaction.** `Store.Receive` takes a transaction-scoped advisory
  lock on the idempotency scope, then re-checks the scope, so concurrent
  duplicates replay instead of racing. It commits payment, instruction, version 1,
  command, idempotency record, audit entry, `PaymentReceived` v1 event, outbox row
  and a `PENDING` `workflow_dispatch` row for `payment/{paymentId}` together.
  Integration tests inject a failure after each of the eight steps and assert
  that no rows remain.
- **Idempotency scope.** The scope is the channel (`CUSTOMER_API`), the request's
  `customerId` (which must be in the token's `customer_ids`) and the
  `Idempotency-Key`. The SHA-256 of the canonical instruction JSON decides between
  replaying the receipt and returning `409`. Records have no TTL. A replay is
  answered from the payment store before any Core Banking call, so a caller can
  recover a lost reply while Core Banking is down.
- **Account authorization.** The payment service never reads Core Banking tables.
  It asks the Core Banking port (`GET /accounts/{id}`) with a client-credentials
  token from a new confidential `payment-service` OIDC client, which has the
  `internal_service` role and the `core-banking-service` audience. The debtor must
  be an open customer deposit owned by `customerId`, in the payment currency.
  Unknown and foreign debtor accounts both return `403`. A book-transfer creditor
  must be an open customer deposit in the same currency. If ownership cannot be
  established, the result is a retryable `503`, never an assumed allowance.
  Reads (`GET /payments/{id}/receipt`, `GET /commands/{id}`) are limited to the
  owning customer scope, or the operator roles `ops_viewer`, `investigator`,
  `approver` and `auditor`.
- **HTTP boundary.** `contracts/openapi/v1/payment.json` (OpenAPI 3.1.1) is
  authoritative. oapi-codegen cannot resolve cross-file references to
  `types.schema.json`, so `contracts.BundleOpenAPI` uses kin-openapi to
  internalize them into `internal/payment/api/payment.bundle.json`. oapi-codegen
  `v2.8.0` then generates the Go 1.22+ `net/http` server boundary from that
  bundle (`make openapi-generate`). The bundle is embedded, and kin-openapi
  `openapi3filter` validates each request after authentication and before the
  handler. A test fails if the bundle is stale. Generated types are mapped to
  domain types; domain validation and authorization still apply.
- **Read endpoints.** Receipt and command reads come from the command store. They
  show pending dispatch before any projection exists. B03 owns the projection
  queries `GET /api/v1/payments` and `GET /api/v1/payments/{id}`.

## Dependencies

Pinned in B02: `github.com/getkin/kin-openapi v0.149.0` (bundling and request
validation), `github.com/oapi-codegen/runtime v1.7.0` (generated-code runtime),
oapi-codegen `v2.8.0` (generator only, run via `go run`), and
`golang.org/x/oauth2 v0.37.0`. The last is an addition to the register: OAuth2
client credentials are protocol mechanics (token fetch, caching and refresh), so
ADR 0007 prefers the maintained Go module to local code. Minimum version selection
raised `santhosh-tekuri/jsonschema/v6` from v6.0.2 to v6.0.3. `govulncheck` found
GO-2026-5970 (infinite loop in `golang.org/x/text`, reached through pgx
connection setup; the previous v0.29.0 was also affected). `golang.org/x/text`
moves to v0.42.0 and `golang.org/x/sync` to v0.23.0, after which the scan reports
no vulnerabilities.

## Consequences

Accepting a new instruction synchronously depends on Core Banking account
lookup. Replays do not. No workflow starts in B02: the dispatch row stays
`PENDING` and visible until B06 adds the leased dispatcher. B04 and B06 build
command records, approvals and cancellation APIs on `Store.Apply` and the claim
primitives rather than writing payment rows directly. Realms imported before B02
must run `make oidc-sync`, which declares `customer_ids` as an admin-only managed
user-profile attribute. Keycloak 26 hides unmanaged attributes from the admin
API, so this declaration is what lets the attribute be maintained.
