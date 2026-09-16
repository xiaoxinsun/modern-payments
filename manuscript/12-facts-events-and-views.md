# Chapter 12 — Facts, Events, and Views

## Business question

When a payment system publishes events, writes logs, updates read models, and advances workflows, what information should consumers actually trust?

Modern architectures often use the word **event** for several very different things. Payments become easier to reason about when we distinguish business facts from internal notifications and operational telemetry.

## Scenario: “payment completed” appears everywhere

Maya's payment completes. GTB now produces several outputs:

- the payment domain records completion;
- an event is published to downstream systems;
- the mobile read model changes to `Completed`;
- a workflow closes;
- logs record processing details;
- metrics increment a success counter.

All of these refer to the same payment, but they do not have the same authority.

## Domain or business events

A domain event represents a meaningful business fact that has occurred.

Examples:

- `PaymentAccepted`
- `PaymentDebited`
- `ExternalPaymentAccepted`
- `PaymentReturned`

A good domain event has a clear meaning independent of the transport used to publish it.

If a consumer asks what `PaymentAccepted` means, the answer should be a business definition, not “it is topic X on Kafka.”

## Internal system events

Systems also publish implementation-level events such as cache invalidations, worker signals, retry notifications, or integration messages.

These may be extremely useful without being part of the enduring business vocabulary.

Keeping them distinct prevents implementation details from becoming accidental enterprise contracts.

## Observability telemetry

Logs, metrics, and traces answer operational questions:

- How long did validation take?
- Which dependency timed out?
- How many payments are waiting?
- Which service instance processed this message?

Telemetry is essential, but it is not the authoritative business record.

A metric counter saying 10,000 payments completed should ultimately be reconcilable to domain and accounting records rather than being treated as financial truth by itself.

## Projections and read models

A read model presents data optimized for a particular audience.

The mobile app may need a simple timeline. Operations may need detailed external references and retry history. Finance may need settlement and reconciliation views.

These are **projections** from authoritative facts. They can be rebuilt if the source data is preserved correctly.

The advantage is flexibility: the core domain does not need to store every consumer's preferred shape.

## Events do not eliminate systems of record

Event-driven architecture can tempt teams to say, “the event stream is the source of truth.” Sometimes an event store genuinely is the authoritative record for a domain. Sometimes it is only an integration channel carrying copies of facts recorded elsewhere.

The architecture should state this explicitly.

For each event, know:

- who is authoritative for the fact;
- whether the event is durable evidence or a notification;
- whether it can be replayed;
- how duplicates are handled;
- whether ordering matters;
- what consumers should do when it conflicts with another source.

## The outbox problem

A common failure occurs when a service updates its database and publishes an event as two separate operations.

The database commit succeeds; the publish fails. Now the business fact exists but consumers were never notified.

Patterns such as the transactional outbox solve this by committing the business change and an outbound-event record in the same local transaction, then publishing asynchronously.

The broader lesson is more important than the pattern name: **if downstream systems depend on an event, the creation of that event must have a deliberate consistency relationship with the business fact it represents.**

## Architecture implication

A clear information model distinguishes:

```text
Business facts
     |
     +--> Domain events
     |
     +--> Customer projection
     |
     +--> Operations projection

Workflow history ----> orchestration/recovery
Telemetry -----------> logs, metrics, traces
Ledger --------------> accounting truth
```

The arrows may differ in a real implementation, but the roles should remain understandable.

## Key takeaway

Not every message is a domain event, not every event is a source of truth, and not every log line is evidence of a business fact.

> **Name information by the role it plays: fact, event, workflow state, projection, or telemetry. Architecture gets clearer immediately.**
