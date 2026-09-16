# Chapter 2 — The Payment Lifecycle

## Business question

What has to happen between a customer's payment request and a payment that everyone can safely treat as complete?

There is no single lifecycle that every rail follows exactly. But engineers still need a reusable mental model. The purpose of a lifecycle is not to force every scheme into one state machine. It is to help us reason about **which business facts have happened and which are still outstanding**.

## Scenario: the same payment seen over time

Maya sends SGD 100 from GTB to Daniel at another bank.

At 09:00:00, the app accepts her input.

At 09:00:01, GTB confirms that the account can be used and that the payment passes its initial checks.

At 09:00:02, GTB records the relevant internal accounting effect and creates an outbound payment instruction.

At 09:00:03, the external infrastructure acknowledges the message.

At 09:00:04, the receiving institution accepts the payment.

At some later point, the interbank obligation is settled according to the rail's rules, and Daniel's bank records the customer credit.

A customer may experience that as “instant.” Architecturally, several facts may have changed underneath.

## A reusable lifecycle

A useful generic lifecycle contains these ideas:

1. **Initiation** — someone creates a payment instruction.
2. **Validation** — the bank checks whether the instruction is structurally and semantically acceptable.
3. **Authorization / controls** — the bank decides whether it is allowed to proceed.
4. **Acceptance** — the bank takes responsibility for processing the instruction under its rules.
5. **Execution** — internal accounting and/or external payment activity begins.
6. **Clearing** — participants exchange, validate, sort, or calculate obligations where the scheme requires it.
7. **Settlement** — the financial obligations between institutions are discharged using the scheme's settlement mechanism.
8. **Completion** — the participant can state that the relevant payment obligations for its scope are fulfilled.

These are concepts, not mandatory API states. A particular payment system may combine or reorder them.

## Clearing is not settlement

The terms **clearing** and **settlement** are often used casually as if they mean the same thing.

They do not.

Clearing concerns the processing and exchange of payment information and, in some systems, the calculation of obligations between participants. Settlement concerns the actual discharge of those obligations.

Imagine GTB owes City Bank SGD 10 million after processing a large batch of customer payments, while City Bank owes GTB SGD 8 million. A clearing process may determine that GTB has a net obligation of SGD 2 million. Settlement is the separate act that discharges that SGD 2 million obligation using the designated settlement asset and system.

Some systems settle each payment individually. Others calculate net obligations over a period. The lifecycle model should leave room for both.

## Completion has a scope

The word **completed** is dangerous without a subject.

GTB may consider its outbound processing complete when it has successfully handed the instruction to an external system. That does not necessarily mean Daniel has been credited.

Daniel's bank may consider its processing complete when it has accepted and posted the credit. A scheme may define another point at which settlement is final. A customer-facing app may simplify several internal states into a single label.

For this reason, status should answer a scoped question:

- Has GTB accepted the instruction?
- Has GTB posted the debit?
- Has the rail accepted it?
- Has settlement occurred?
- Has the beneficiary bank accepted it?
- Has the beneficiary been credited?

A payment lifecycle is therefore closer to a set of related facts than a single universal status field.

## Pending is a legitimate business state

Many systems treat `PENDING` as something embarrassing: a temporary technical state to eliminate.

In payments, pending may be the most accurate description available.

The bank may know that the instruction was accepted but not yet know whether the external leg succeeded. It may know that a rail accepted a message but not yet know the settlement result. It may be waiting for a scheme-defined return window or manual review.

The architecture must be comfortable with uncertainty. Replacing uncertainty with premature success is worse than exposing a carefully defined pending state.

## State transitions should follow facts

A robust lifecycle moves forward because new evidence arrived, not merely because time passed or a function returned without throwing an exception.

For example:

```text
RECEIVED
  -> VALIDATED
  -> ACCEPTED
  -> SENT_EXTERNALLY
  -> EXTERNAL_ACCEPTED
  -> COMPLETED
```

This is useful only if each transition has a business meaning. If `SENT_EXTERNALLY` means “our HTTP client returned 200,” then the name may be misleading. If `EXTERNAL_ACCEPTED` means “the receiving scheme accepted the instruction for processing,” say so.

States should be named after facts that operators and engineers can defend.

## Architecture implication

The lifecycle influences several design choices:

- A payment record needs enough history to explain how the current state was reached.
- External acknowledgements should be correlated to the original instruction.
- Customer status should be a projection from richer internal facts, not the only stored truth.
- Timeout handling must preserve uncertainty rather than inventing failure or success.
- State changes that have financial consequences should be auditable.

It is often useful to model the payment as an explicit state machine, but the state machine should not become a substitute for the underlying facts. A state such as `SETTLED` should be backed by evidence that the system can point to later.

## Mental model

Think of a payment lifecycle as a **progressive accumulation of evidence**.

At the beginning, the bank only knows what the customer requested. As the payment proceeds, the bank learns more: checks passed, accounting posted, external infrastructure accepted, settlement occurred, beneficiary accepted.

The lifecycle is the structured story of those facts.

## Key takeaway

A payment does not jump from “requested” to “done.” It accumulates business facts across several participants and systems.

> **Model the lifecycle as evidence about what has happened, not as one magical status field that tries to mean everything.**
