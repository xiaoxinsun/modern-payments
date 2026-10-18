# Payments primer

The domain ideas behind B01–B03, each tied to the code that implements it. The
authoritative rules are in `spec/gtb-foundation.md` (Sections 5 and 7) and ADRs
0008–0010. This page is the short version.

## The cast

- **Customer**: Maya (customer `20000000-…-0001`) owns deposit account
  `10000000-…-0001`. Daniel owns `…-0002`.
- **Payment service** (`payment-api`, `payment-worker`): takes instructions,
  decides what happens to them, and records every decision. It owns *no money*.
- **Core Banking** (`core-banking-service`): the ledger, and the only authority
  on balances. The payment service asks it to reserve and move money, and it
  never reads its tables.
- **Rails** (instant, RTGS, cross-border; B07–B09): the external networks that
  move money between banks. Simulated later.
- **Query side** (`event-projector`, `query-api`): read-only views built from
  events, for customers and operators.

## Money

- **Minor units, never floats.** SGD 25.00 is `2500`. A float cannot represent
  0.10 exactly, and rounding errors in money are defects. On the wire it is
  `{"amountMinor":"2500","currency":"SGD"}`. The amount is a *string*, because
  JavaScript numbers lose precision above 2^53.
  → `backend/internal/platform/value/money.go`, [slice 01](rosetta/01-money-and-ids.md)
- **Currency travels with the amount.** Adding SGD to USD is an error, not a
  conversion. No foreign-exchange conversion exists in this system.

## The ledger (B01)

- **Double entry.** Every posting has debit and credit lines, and they balance
  per currency: Maya's account is debited 1000 and Daniel's credited 1000. Money
  is never created or destroyed by a posting.
  → `corebanking/domain/model.go` `ValidateEntries`
- **Account kinds and sides.** A customer deposit is a *liability* of the bank.
  A credit increases it, and a debit decreases it (with no overdraft). The
  settlement (nostro) account is an *asset*, where a debit increases it.
  → `domain.BalanceAfter`
- **Booked vs. available.** Booked is the sum of posted entries. Available is
  booked minus active **holds**. A hold reserves money so that two payments
  cannot spend the same funds. A hold ends exactly once: `CONSUMED` by a
  posting, or `RELEASED`.
- **Append-only history.** Posted entries are never edited. A mistake is fixed
  with a **reversal**, a new posting that mirrors the original and links to it.
  Database triggers reject `UPDATE`/`DELETE` on journals.
  → `backend/migrations/corebanking/0002_*.sql`, `Service.Reverse`
- **Operation IDs and lost replies.** Every ledger call carries a caller-chosen
  `operationId`.
  - Repeat it with the same payload, and you get the original result back.
  - Repeat it with a different payload, and you get `409`.
  - If a reply is lost (a timeout), ask `GET /operations/{id}` what happened.
    **Never invent a new ID to retry**: that is how banks double-post.
  → [slice 02](rosetta/02-ledger-posting.md)

## A payment's life (B02)

- **Received is not accepted.** `POST /payments` returns `202` when the
  instruction is durably stored: every row that describes it committed in one
  database transaction. That promises only "we have it and will process it".
  Validation, screening, limits and execution come later, in a workflow (B06).
- **Idempotency key.** A customer's app may retry a timed-out submit. The
  `Idempotency-Key` header, scoped to channel + customer, turns the retry into
  a replay of the original receipt. The same key with a different payload is
  `409`. Keys never expire (foundation Section 7).
  → [slice 04](rosetta/04-durable-receipt.md)
- **Two state dimensions.**
  - The **lifecycle** (`RECEIVED → VALIDATING → ACCEPTED → EXECUTING →
    COMPLETED`, plus `REJECTED`, `CANCELLED`, `RETURNED`) is the business view.
  - The **execution outcome** (`NOT_SUBMITTED → PENDING → CONFIRMED_SUCCESS /
    CONFIRMED_FAILURE / UNKNOWN`) is what the rail told us.
  - They are separate because "we sent it" and "we know what happened" are
    different facts.
  → `payment/domain/payment.go`, [slice 03](rosetta/03-payment-state-machine.md)
- **Unknown stays unknown.** If a rail call times out, the money may or may not
  have moved. The payment goes to `UNKNOWN`, and only *evidence* (a status
  query, a report, a callback) moves it on. Treating a timeout as a failure
  and refunding the customer is a classic, expensive bug. The domain refuses
  to `REJECT` while the outcome is `PENDING` or `UNKNOWN`.
- **Evidence, actor, policy.** Every state change records:
  - who did it (actor);
  - under which rule set (policy version);
  - why (reason);
  - and on what evidence.

  Auditors and investigators need this history.
  → `payment_versions` table
- **Claims: cancel vs. submit.** A customer cancelling while the workflow
  submits to the rail is a race. One exclusive **claim** decides it: whichever
  commits first wins, and the other gets a conflict. Money can then never be
  both cancelled and sent.
- **Optimistic versions.** Each change names the version it expects. A stale
  writer gets `ErrVersionConflict` instead of silently overwriting.

## Events, outbox and queries (B03)

- **Why events.** Other parts of the bank (notifications, reporting, AML
  monitoring, dashboards) need to know what happened without querying the
  payment database. The payment service publishes **domain events** such as
  `PaymentReceived`.
- **The dual-write problem and the outbox.** "Commit to the database, then
  publish to Kafka" loses events when the process dies in between. Instead, the
  event is written to an **outbox** table *in the same transaction* as the
  change. A **relay** later publishes it and marks it delivered only after
  Kafka acknowledges.
  → [slice 06](rosetta/06-outbox-relay.md)
- **At least once, so consumers deduplicate.** Relays may publish an event
  twice (after a crash or a retry). Every event has a stable `eventId`, and
  consumers keep an **inbox** of the IDs they have processed.
- **Ordering.** Events are keyed by payment ID, so one payment's events share a
  Kafka partition and arrive in order. Retries can still reorder them, so each
  event carries an `eventSequence`, and the projector buffers anything that
  arrives early.
  → [slice 07](rosetta/07-projector.md)
- **CQRS.** Commands (writes) and queries (reads) use different models,
  databases and credentials. The query side is *eventually consistent*: it can
  lag. Every query answer therefore says how fresh it is, and a payment not
  yet projected shows as `PENDING` from the command store rather than "not
  found".
- **Read models never authorize money movement.** A projection can be stale,
  so no financial decision reads from it.
- **Rebuild.** The events in the payment database (the journal) are the source
  of truth, and Kafka is only transport. A projection can be thrown away and
  rebuilt from the journal while traffic continues. Rebuilding must never
  resend money or notifications.

## Glossary

| Term | Meaning here |
| --- | --- |
| Instruction | What the customer asked for: debtor, creditor, amount, product |
| Product | `BOOK_TRANSFER` (inside the bank), `DOMESTIC_INSTANT`, `RTGS` (high value), `CROSS_BORDER` |
| Book transfer | Both accounts are at GTB, so the ledger alone settles it |
| Rail | An external payment network |
| Nostro / settlement account | GTB's account at the central bank or a correspondent |
| Suspense | A holding account for money that is not yet attributable |
| Correlation ID | Ties together everything caused by one request, across services |
| Causation ID | The immediate command or event that caused this one |
| Aggregate | One consistency unit, here one payment; its version increments by one per change |
| Projection / read model | A query-optimized copy derived from events |
