# ADR 0010: Event delivery and CQRS query foundation

Date: 2026-09-27. Status: accepted for B03.

## Context

Foundation Section 7 requires outbox relays that publish at least once and mark
delivery only after broker acknowledgement. It also requires aggregate-keyed
partitions, and consumers that commit deduplication, projection changes and
checkpoint atomically. Ordering gaps need bounded buffering with journal
catch-up, and poison messages go to a visible repair queue. Rebuilds use
shadow tables from a journal watermark and never replay effects. Queries report
`asOf`, the projected version and freshness, and an unprojected receipt shows as
pending. Section 11 requires Kafka outages to leave events in the outbox, with
lag made visible and redelivery deduplicated. B02 left payment `domain_events`
and `outbox`, and B01 left `posting_events` and `posting_outbox`, both
unpublished.

## Decision

- **Kafka mechanics** use `github.com/twmb/franz-go v1.22.0` (register row B03).
  - Topics are created explicitly by `make kafka-topics`:
    - `gtb.payment.events.v1` and `gtb.corebanking.postings.v1`;
    - 3 partitions each, 7-day retention.
  - Records are keyed by aggregate ID. The body is the v1 envelope, and headers
    duplicate `eventId`, `eventType` and `schemaVersion`.
  - Kafka is not a system of record. The PostgreSQL journals are, so finite
    retention is safe.
- **Relays** (`internal/eventing.Relay`) are project code.
  - Placement:
    - the payment relay runs in `payment-worker`, which serves health only
      until B06 adds workflows;
    - the posting relay runs inside `core-banking-service`.
  - Each owner's `Deliver` locks a batch of undelivered outbox rows in journal
    order with `FOR UPDATE SKIP LOCKED`, publishes them, and then records each
    result in the same transaction. A published row gets `published_at`; a
    failed one gets `delivery_attempts + 1` and a bounded `last_error`.
  - A crash before commit republishes the same event IDs. Kafka availability
    is not a readiness check, and failures back off exponentially up to 10s.
  - The producer is idempotent with acks=all, but it uses
    `AllowIdempotentProduceCancellation` and a 10s delivery timeout. Without
    that, franz-go refuses to fail in-flight records, which would hold a
    relay's outbox transaction open for as long as the broker is down.
  - A relay retry may therefore write an event twice. That is within the
    at-least-once contract, and the consumer inbox absorbs it.
- **Event payloads.**
  - `Store.Apply` stamps the resulting `paymentId`, `lifecycleState` and
    `executionOutcome` into every payment event, so projections never infer
    state from event names.
  - Events journaled before B03 are upcast when read by the relay or the
    journal API: missing fields come from the `payment_versions` row committed
    in the same transaction. The stored journal is never rewritten.
  - Payment migration `0003` adds a global `journal_position` identity for
    rebuild watermarks and catch-up paging.
- **Journal API.**
  - payment-api exposes read-only endpoints, restricted to the
    `internal_service` role:
    - `GET /integrations/v1/payment-events/watermark`;
    - `GET /integrations/v1/payment-events?afterPosition&upToPosition&limit`;
    - `GET /integrations/v1/payments/{id}/events?afterSequence`.
  - The projector calls them with its existing `event-projector` identity and
    never reads payment tables.
  - Positions are allocated at insert, so a lower position can become visible
    later. Readers combine the journal with live delivery and deduplicate by
    event ID.
- **Projections** (projections migration `0002`, `event-projector`).
  - Every projection table is keyed by generation. Reads join the single
    `ACTIVE` generation.
  - The live consumer projects each record into every open generation (ACTIVE
    and any BUILDING shadow). One transaction per poll batch covers:
    - the inbox (`generation, event_id`);
    - `payment_summary` and `payment_timeline`;
    - the ordering-gap buffer (`pending_events`);
    - the repair queue;
    - the Kafka checkpoint (`consumer_offsets`).
  - Kafka offsets are then committed only for tooling. On partition assignment
    the database checkpoint wins, and a partition with no checkpoint restarts
    from the log start.
  - Gaps and poison:
    - An event ahead of the expected sequence is buffered.
    - Filling the gap drains the buffer in order.
    - Buffers older than 5s, or holding 100 events for one payment, are filled
      from the journal as REPLAY deliveries.
    - A buffered event absent from the journal is quarantined.
    - Undecodable records, foreign aggregate types, unrepresentable payloads,
      schema-rejected rows (inside a savepoint) and conflicting sequence
      identities all go to `repair_queue` instead of blocking the partition.
  - The consumer group session timeout is 10s, so a crashed projector's
    partitions are reassigned quickly.
- **Rebuild** (`event-projector rebuild`):
  1. Create BUILDING generation G+1. Live events now reach it too.
  2. Read the journal watermark W.
  3. Replay the journal up to W into G+1 as REPLAY deliveries.
  4. Settle any buffered gaps.
  5. Verify, for positions up to W:
     - the payment and event counts;
     - that no gaps remain;
     - that up to 20 recent and 20 random timelines are prefix-identical to the
       journal.
  6. Switch in one transaction: ACTIVE becomes RETIRED, and G+1 becomes ACTIVE.
  7. Delete the retired rows.

  A failed verification marks G+1 FAILED and leaves reads untouched. Only one
  BUILDING generation may exist; `-abandon-building` clears a crashed one.
- **Replay suppression.** `eventing.Consumer.Accepts` withholds REPLAY
  deliveries from any consumer marked effectful. Rebuild and gap catch-up are
  REPLAY by construction, and the projector holds no command or ledger
  credentials.
- **Query API** (`query-api`, `contracts/openapi/v1/query.json`).
  - Endpoints:
    - `GET /api/v1/payments` (keyset pagination; the cursor is bound to caller
      and filters);
    - `GET /api/v1/payments/{id}` (`minVersion` returns BEHIND until reached);
    - `GET /api/v1/payments/{id}/timeline`;
    - `GET /api/v1/ops/projections` (generations, lag, gap buffer, repairs).
  - Every response carries `projection` metadata: `asOf`, `generation`,
    `freshness` and `consumerLag`.
  - Freshness is:
    - STALE when the consumer has not reached Kafka within 10s (broker or
      projector outage);
    - LAGGING when a known backlog exists;
    - otherwise CURRENT.
  - Scope matches the receipt read:
    - customers need `customerId` in their token scope and must name it when
      searching;
    - `ops_viewer`, `investigator`, `approver` and `auditor` may read all
      payments;
    - `platform_admin` may also read the status endpoint.
  - An unprojected payment is answered from the command store: query-api
    forwards the caller's own bearer token to
    `GET /api/v1/payments/{id}/receipt` and returns `status: PENDING`. The
    owner still authorizes the read.
  - query-api validates the `payment-api` audience, because commands and
    queries are one public `/api/v1` surface.
- **Read credentials.** query-api connects as a new `query_reader` login. It is
  provisioned by `make db-provision`, can connect only to `projections`, is
  read-only by default, and has SELECT grants that exclude raw poison records,
  buffered envelopes and the inbox.

## Consequences

- Payments now have a customer and operator query surface that can lag the
  command store, and says so.
- Receipts stay authoritative, and pending reads depend on payment-api being
  available.
- The Core Banking posting events are published but not yet projected; B06 and
  B10 consume them.
- Effectful consumers (B15 notifications) must register as effectful and keep
  their own inbox.
- A `make infra-reset` or new stack needs `make kafka-topics`. An existing
  stack needs `make infra-init db-provision db-migrate kafka-topics` for the new
  reader credential, grants and topics.
- The walkthrough projector shares the consumer group with the smoke, so stop
  it before running `make eventing-smoke`.
