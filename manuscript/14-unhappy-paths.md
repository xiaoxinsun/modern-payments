# Chapter 14 — When a Payment Does Not End Happily

## Business question

What does the bank do when a payment is rejected, cancelled, reversed, returned, refunded, repaired, or investigated?

These words are often used interchangeably in casual conversation. In payment systems they can represent very different business events.

## Scenario: Daniel cannot receive the payment

Maya sends SGD 100 to Daniel. Depending on when the problem is discovered, different outcomes are possible.

If GTB discovers before execution that the beneficiary details are invalid, it may **reject** the instruction.

If Maya asks to stop a payment before it has passed the point where cancellation is allowed, GTB may **cancel** it.

If a financial entry was posted and later needs to be undone, the bank may create a **reversal** or another compensating entry.

If Daniel's bank receives the payment but later sends it back under scheme rules, that is a **return**.

If Maya paid a merchant and the merchant later sends money back as a new business transaction, that may be a **refund**.

The exact terminology depends on the product and rail, but the distinctions matter.

## Rejection

A rejection normally means the instruction was not accepted for the requested processing.

Examples:

- invalid account;
- unsupported currency;
- insufficient funds;
- failed eligibility checks;
- scheme validation failure.

A rejection may happen before any customer debit or after some temporary reservation that then needs release.

## Cancellation

Cancellation is a request to stop a payment that has not yet crossed a point of no return under the relevant rules.

The architecture must know whether cancellation is still possible. A UI button cannot guarantee it simply because the payment appears pending.

Cancellation may itself become a workflow with an uncertain outcome if the payment has already been sent externally.

## Reversal

A reversal is commonly used to counteract a prior financial effect.

Instead of deleting the original debit, the bank records a new accounting entry that reverses it. This preserves the financial history.

The original payment and the reversal should remain linked.

## Return

A return usually means a payment that was previously accepted or processed is sent back according to scheme rules.

The return may occur after the original payment looked complete to the sender. That means customer-facing status may need to evolve from `Completed` to `Returned` rather than pretending the original event never happened.

## Repair and investigation

Some payments cannot be resolved automatically.

Missing information, mismatched references, external disputes, or ambiguous outcomes may require an operations team to investigate.

A repair queue should not be a graveyard of broken transactions. It should be a managed workflow with ownership, evidence, deadlines, and allowed actions.

## Architecture implication

Model unhappy paths as first-class domain flows.

For each outcome, define:

- what business fact occurred;
- what accounting action is required;
- what external message or scheme process applies;
- whether customer status changes;
- whether the original payment remains immutable;
- what audit trail is needed.

Avoid a generic `FAILED` bucket for everything.

## Key takeaway

A payment can stop, be undone, come back, or require human repair in several fundamentally different ways.

> **Model exceptions with the same care as the happy path, because financial history must remain explainable after something goes wrong.**
