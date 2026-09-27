# Event contracts, version 1

`envelope.schema.json` defines the shared event metadata and generic object payload.
Go's `internal/contracts.Event` mirrors this shape. Aggregate versions and event
sequences use decimal strings; timestamps are UTC. Feature phases add typed
payloads, transactional persistence, sequencing and delivery. See ADR 0002.

Typed payloads: `core-banking-posting-committed.schema.json` (B01) and
`payment-received.schema.json` (B02). `PaymentReceived` means durably received,
never accepted; it carries no account or beneficiary details.

Delivery (B03, ADR 0010): outbox relays publish envelopes to Kafka topics
`gtb.payment.events.v1` and `gtb.corebanking.postings.v1`, keyed by
`aggregateId`, at least once with stable event IDs; consumers deduplicate by
event ID and use `eventSequence` to detect gaps. Every payment event payload also
carries the resulting `paymentId`, `lifecycleState` and `executionOutcome`;
events journaled before B03 are upcast with those fields when read.
