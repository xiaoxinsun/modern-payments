# Chapter 10 — Retrying Without Paying Twice

## Business question

How can a payment system retry failed work without accidentally creating a second financial effect?

Retries are unavoidable in distributed systems. Networks fail, services restart, responses are lost, and operators replay messages. In payments, the problem is not whether retries happen. The problem is whether repeated attempts are **safe**.

## Scenario: Maya taps twice

Maya presses Send. The app appears to freeze, so she presses Send again.

Two HTTP requests reach GTB.

Are they two payment instructions or one instruction delivered twice?

The system cannot answer from timing alone. It needs an identity model.

## Idempotency

An operation is idempotent when repeating the same logical request produces the same intended business effect rather than duplicating it.

For a payment API, the client might submit an idempotency key:

```text
customer_request_id = 9d2...
```

GTB can then recognize a repeated submission and return the result of the original logical instruction.

This works only if the key has a defined scope and retention policy. “We store it for a while” is not enough when duplicate financial effects are possible.

## Deduplication is broader than API idempotency

Duplicate messages can arise at many layers:

- customer retries;
- message-broker redelivery;
- workflow replay;
- external scheme retransmission;
- operator reprocessing;
- batch-file resubmission.

Each layer may require its own duplicate identity.

For example, an inbound rail message may carry a scheme-specific unique reference. GTB should not assume that its customer-facing idempotency key is relevant at that boundary.

## Exactly once is usually a business property

Engineers sometimes seek “exactly-once delivery.” Transport guarantees alone are rarely enough to guarantee exactly one financial effect across independent systems.

A more useful target is:

> **At-least-once attempts + durable deduplication + idempotent business effects = effectively one intended financial result.**

This shifts the problem from wishful transport semantics to explicit domain design.

## The idempotency record is part of financial safety

Suppose GTB receives the same request key twice. The second request should not simply be rejected as an error. It may need to return the original payment ID and current result.

The idempotency store therefore needs to record enough information to establish that both attempts refer to the same logical instruction.

A dangerous design accepts the same key with different amounts or beneficiaries. The system should detect conflicting reuse rather than silently returning an unrelated result.

## Side effects need individual identities

A payment may perform several external effects:

- reserve funds;
- post debit;
- send rail instruction;
- charge fee;
- send notification.

If the workflow retries, each financially meaningful side effect should have a stable identity or another mechanism that prevents duplicate application.

A single top-level payment ID helps, but the accounting system and rail may need their own reference keys.

## Architecture implication

Design idempotency deliberately at every boundary:

| Boundary | Example duplicate key |
|---|---|
| Client → GTB | customer request ID |
| Payment → ledger | posting reference |
| Payment → rail | outbound message / transaction ID |
| Rail → GTB | scheme message ID |
| Workflow activity | activity/business operation ID |

The exact identifiers differ, but the principle is the same: retries should rediscover previous work rather than blindly repeat it.

## Key takeaway

Retries are not exceptional behavior. They are part of normal distributed payment processing.

> **Design every important side effect so the system can tell the difference between “try again” and “do it again.”**
