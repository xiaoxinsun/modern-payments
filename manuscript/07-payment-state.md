# Chapter 7 — What State Is the Payment In?

## Business question

When different systems disagree about a payment, which state should the bank believe?

The better question is: **which fact is each system responsible for?** A payment has several dimensions of state, and forcing them into one column creates ambiguity.

## Scenario: one payment, four answers

Maya asks GTB to send SGD 100 to City Bank.

GTB's ledger says Maya has been debited.

The payment service says the outbound instruction was sent.

The external rail says the instruction is pending.

The mobile app shows “Processing.”

All four can be correct at the same time.

## Business state

Business state describes the payment as understood by the bank's domain model.

Examples might include:

- received;
- accepted;
- executing;
- completed;
- rejected;
- returned;
- under investigation.

These states summarize business meaning. They should be supported by underlying facts rather than being the only evidence stored.

## Accounting state

Accounting state answers questions such as:

- Was the debit posted?
- Is there a reservation?
- Was a fee posted?
- Was a reversal recorded?
- What settlement or suspense accounts were affected?

Accounting state is not derived from the payment service's optimism. It comes from the accounting system.

## External-rail state

The rail may expose its own lifecycle and codes. Those states should normally be preserved rather than translated destructively into the bank's internal status.

For example, an external `ACCEPTED` might mean “accepted for processing,” not “beneficiary credited.” If GTB maps both to `SUCCESS`, information is lost.

A safer pattern is to store the original external status and separately derive the internal business interpretation.

## Workflow state

A workflow engine may know that the next action is `PollExternalStatus` or `WaitForCallback`.

That is implementation state. It is useful for recovery, but it is not automatically a business fact.

This distinction matters because workflow technology changes more easily than the bank's business semantics. A migration from one orchestrator to another should not redefine what “settled” means.

## Customer-visible state

Customers need understandable status, not the full internal state machine.

A product might expose:

- Processing
- Completed
- Failed
- Returned

These are projections from richer state. The projection rules should be explicit. “Processing” might correspond to several internal situations, each of which operations can inspect separately.

## Observability is not business state

Logs, metrics, and traces tell engineers what the software did. They are essential for diagnosis but should not become the only proof that a financial event occurred.

A log line that says `payment completed` is not equivalent to an accounting record or external acknowledgement.

This is a recurring rule:

> **Telemetry describes system behavior. Domain records describe business facts.**

## State changes should be explainable

For every important state transition, operators should be able to answer:

- What evidence caused the transition?
- Which system produced that evidence?
- When did it happen?
- Can the evidence be replayed or inspected?
- What financial entries are associated with it?

If the answer is “the service set the field,” the design is probably too weak for important payment states.

## Architecture implication

A practical payment model often contains several linked records rather than one giant status:

```text
Payment
  - business lifecycle
  - customer-visible projection
  - references to accounting entries
  - external instruction / external statuses
  - workflow correlation
  - audit history
```

The implementation may use one database or many. The conceptual boundaries still help prevent accidental coupling.

## Key takeaway

There is no single universal payment state because different systems own different facts.

> **Separate business state, accounting state, external state, workflow state, customer projections, and observability. Then define how they relate.**
