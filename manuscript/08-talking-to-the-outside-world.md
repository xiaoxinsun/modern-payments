# Chapter 8 — Talking to the Outside World

## Business question

How should a bank communicate with external payment infrastructures without allowing each rail's quirks to infect the whole platform?

The challenge is not simply converting JSON to XML. External integration is where GTB crosses institutional and scheme boundaries. Message semantics, identifiers, acknowledgements, timeouts, and operating rules all become part of the payment's business behavior.

## Scenario: one payment, two representations

Inside GTB, Maya's payment might be represented as a domain object:

```text
amount: 100 SGD
payer: Maya account
beneficiary: Daniel account at City Bank
requested_service: instant transfer
```

The external rail may require a richer, scheme-specific message with its own identifiers, party structure, purpose codes, timestamps, and validation rules.

GTB therefore needs a boundary between its internal model and the rail model.

## Canonical model versus rail model

A canonical internal model is useful because it gives GTB a stable business vocabulary. But it should not pretend that every rail is identical.

If the canonical model contains only the least common denominator, important scheme information is lost. If it attempts to contain every field from every rail, it becomes an unreadable mega-schema.

A better design keeps:

- a stable core payment intent;
- rail-specific extensions where needed;
- explicit transformation rules;
- the original inbound and outbound messages for audit where policy allows.

## Identifiers are part of the protocol

External payment processing often depends on several identifiers:

- GTB's internal payment ID;
- customer-provided references;
- message IDs;
- rail transaction IDs;
- end-to-end identifiers;
- settlement or clearing references.

Do not collapse them into one `transaction_id` field.

Identifiers support deduplication, tracing, reconciliation, investigations, and returns. Their scope and uniqueness rules matter.

## Acknowledgement is not completion

Suppose GTB sends a message and receives:

```text
ACCEPTED
```

That may mean only that the message was structurally valid and accepted for further processing.

The adapter should preserve the external meaning rather than telling the rest of GTB that the payment is complete.

This is particularly important in messaging networks such as SWIFT, where secure financial messages coordinate activities between institutions but the messaging network itself should not be confused with the underlying settlement of funds.

## ISO 20022 as a business vocabulary

Modern payment infrastructures increasingly use ISO 20022 messages because they can carry structured financial information and shared business concepts.

For engineers, the main lesson is not “learn every XML element.” It is that payment messages encode a domain model: parties, accounts, agents, amounts, remittance information, references, status reasons, and more.

The bank's internal model should preserve the business meaning of those concepts even if the internal serialization differs.

## Adapter responsibilities

A rail adapter commonly handles:

- message construction and parsing;
- transport/security requirements;
- scheme validation;
- correlation identifiers;
- acknowledgement/status mapping;
- retries where allowed;
- duplicate detection support;
- operational metrics;
- preserving raw external evidence where required.

What it should avoid is silently deciding domain meaning that belongs in the payment layer.

For example, the adapter can report `rail_status = ACCEPTED`. The payment domain decides what that means for GTB's business lifecycle.

## Architecture implication

External boundaries benefit from an anti-corruption layer:

```text
GTB payment domain
      |
      v
Rail adapter
  - map
  - validate
  - secure
  - send/receive
      |
      v
External scheme / institution
```

This reduces coupling but does not remove the need for rail-specific knowledge. The adapter is precisely where that knowledge should be concentrated.

## Key takeaway

External payment integration is a business boundary, not merely a serialization problem.

> **Preserve rail semantics, identifiers, and evidence. Translate deliberately, and never confuse message acceptance with movement or settlement of value.**
