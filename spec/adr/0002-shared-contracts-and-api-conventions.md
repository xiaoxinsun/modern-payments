# ADR 0002: Shared value types and boundary conventions

Date: 2026-09-26. Status: accepted for I01.

## Context

Foundation Sections 2, 4, 8 and 12 require independently owned services with
versioned contracts. Section 5's money invariant and Section 7's event publication
and identity rules resolve the concrete shared-type requirements for this phase.
These conventions establish contracts; feature phases implement their operations.

## Money and identity

`internal/platform/value.Money` uses signed `int64` minor units with supported
currencies `SGD` and `USD`, both scale 2. The range is symmetric:
`-999999999999999999` through `999999999999999999` minor units. Keeping the limit
at 18 decimal digits permits checked addition without overflowing int64 and a
simple exact schema bound. No float, implicit rounding or FX conversion exists.
Zero and negatives are valid shared amounts (balances/adjustments); payment
submission must additionally require a strictly positive amount. Each boundary
validates currency and bounds; exported Go fields must be validated before use.

JSON uses `{ "amountMinor": "10000", "currency": "SGD" }`. Decimal strings preserve
precision in JavaScript and JSON tooling. Canonical syntax excludes plus signs,
leading zeroes, negative zero, fractions, exponents and whitespace. The Go codec
rejects malformed/missing/unknown money fields without mutating the destination.
`Money.Add` rejects currency mismatch and out-of-range inputs/results.

`value.ID` is a canonical lowercase UUIDv4 generated from cryptographic randomness.
Its generation and parsing delegate to the maintained `gofrs/uuid/v5` package;
the GTB wrapper enforces the narrower version/canonical-form contract.
It is the common representation for internal payment, command, operation, event,
account, customer, correlation and causation identifiers; names of fields retain
their semantic role. It neither encodes order nor grants authority. External
message references and OIDC subjects remain distinct opaque strings, never
silently coerced into UUIDs. A logical operation persists its ID once and reuses it
on retry; fresh IDs are not a deduplication strategy. Owner-specific identifier
wrappers can be introduced with their domain model.

## Envelopes and schema evolution

JSON field names use camelCase, matching foundation Section 7. Shared definitions
are in `contracts/common/v1/types.schema.json`; event and simulator envelopes
reference them. Schemas use JSON Schema draft 2020-12. Unknown envelope fields
are rejected; arbitrary payload objects are placeholders for feature-specific
schemas, not permission to publish unvalidated or protected data. Examples are
synthetic shape fixtures, not evidence of a completed payment flow.

The event envelope includes `eventId`, `eventType`, `schemaVersion`,
`aggregateType`, `aggregateId`, `aggregateVersion`, `eventSequence`, `occurredAt`,
`recordedAt`, `correlationId`, `causationId`, `source` and `payload`.
Version 1 requires all fields. Timestamps are nonzero UTC RFC3339 strings ending
in `Z`, with at most nanosecond precision. Producers normalize timestamps before
serialization. Aggregate versions and event sequences are positive decimal
strings bounded to 18 digits. `eventSequence` is strictly increasing per aggregate
even when multiple events share one aggregate version. Event IDs and sequence
positions remain stable across at-least-once redelivery. Producers partition by
aggregate ID; consumers deduplicate and detect gaps. These are contract semantics;
journaling, transactions and ordering enforcement arrive in B03.

`correlationId` identifies the root interaction, while `causationId` identifies the
immediate durable command/event that caused the fact. External ingress maps to an
internal durable identity. Neither substitutes for tracing, idempotency or auth.
Payloads contain minimal references; compliance evidence stays behind restricted
interfaces. Replay never authorizes a new financial effect.

The generic simulator message carries `schemaVersion`, bounded opaque
`messageId`, `source`, `messageType`, `correlationId`, `occurredAt` and `payload`.
Deduplication scopes message ID by authenticated sender/rail. Product phases
define instructions/evidence and payload schemas before using this envelope.

Breaking wire changes require a new versioned schema and compatible consumers or
upcasters; do not silently edit a published v1 shape. New feature payloads can be
added under v1 without changing these shared definitions.

## API conventions

`contracts/openapi/v1/shared.json` is an OpenAPI 3.1.1 reusable component library
with empty paths. No endpoint is advertised as implemented in I01. Commands and
queries use `/api/v1`; adapter ingress uses `/integrations/v1`. Bodies are JSON
with UTF-8, bounded request sizes and strict schema validation at the boundary.
Domain packages must never import wire-envelope or transport packages.

OIDC bearer access tokens identify callers; I03 defines issuer/audience validation
and service credentials. Each feature declares its required customer/account or
operator scope and enforces it server-side, including list, cursor and detail
access. IDs, headers and customer IDs supplied in bodies are not authorization.

`X-Correlation-ID` is an optional incoming UUIDv4; generate one when absent and
echo a valid identifier on all responses, including errors. Invalid supplied
values produce `400` with a newly generated safe correlation ID. Never echo
unchecked input into logs or headers.

Durable mutation commands require `Idempotency-Key`: 1–128 characters from
`[A-Za-z0-9._:-]`. Its scope includes channel plus authenticated customer/actor;
the canonical payload hash distinguishes retries from conflicting reuse. Same
identity and payload return the original receipt, conflicting reuse returns
`409`. Financial duplicate identities have no automatic TTL. Canonical hashing,
receipts and transactional deduplication are B02 work.

Commands return committed receipts with `commandId`, relevant aggregate ID and
committed version; asynchronous acceptance uses `202`, not a claim of payment
completion. Queries later expose `asOf`, projected version and freshness and must
handle projection lag. List requests use opaque `cursor` (maximum 2048 chars)
and `limit` (1–100, default 50); reject invalid limits, bind cursors to scope and
filters, use a stable ordering, and return `nextCursor` only when another page
exists. Pagination storage/encoding is feature work.

Errors are `{ "error": { "code", "message", "retryable" }, "correlationId" }`.
Messages contain 1–256 characters of safe public text. Do not include stack
traces, account existence leaks, screening details or raw provider responses.

| Code | HTTP status |
| --- | --- |
| `INVALID_REQUEST` | 400 |
| `UNAUTHENTICATED` | 401 |
| `FORBIDDEN` | 403 |
| `NOT_FOUND` | 404 |
| `CONFLICT` | 409 |
| `RATE_LIMITED` | 429 |
| `DEPENDENCY_UNAVAILABLE` | 503 |
| `INTERNAL_ERROR` | 500 |

`retryable` describes an API request retry with the same durable identity only.
HTTP errors and timeouts cannot establish an external financial outcome. Unknown
effects require lookup/reconciliation, never blind resubmission or compensation.
Add `Retry-After` for bounded throttling/unavailability where known. Feature
contracts specify adapter timeouts, capabilities, evidence and retry policy.

## Validation and limits

Go tests compare monetary schemas with Go decoding, validate example and invalid
envelopes, compile references, and validate the OpenAPI document against its
vendored official schema. Go envelope users must call `Validate`; schema
validation of untrusted wire input remains mandatory at future ingress handlers.
Generic payload schemas do not validate future business messages. No running
infrastructure, endpoints, auth enforcement or business flows are claimed here.
