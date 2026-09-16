# Chapter 9 — What If Something Fails Halfway?

## Business question

What should the bank do when a payment has partially succeeded and the software no longer knows the final outcome?

This is the moment where payment processing becomes a distributed-systems problem.

## Scenario: the timeout

GTB debits Maya's account and sends an external payment instruction to City Bank through an instant-payment rail.

The request leaves GTB.

City Bank receives it and processes the payment.

The response is lost on the network.

GTB sees a timeout.

What happened?

From GTB's local perspective, the answer is **unknown**. Retrying may be necessary—or may pay Daniel twice if the rail does not deduplicate the request.

## Failure is not one state

Software often simplifies outcomes into success or failure. Payments need at least a third category:

- definitely succeeded;
- definitely failed;
- **outcome unknown**.

Unknown is uncomfortable, but it is honest.

A timeout proves that GTB did not receive a timely response. It does not prove that City Bank failed to process the instruction.

## Partial success is normal

Consider a simplified sequence:

1. create payment record;
2. post debit;
3. send external message;
4. receive acceptance;
5. mark business lifecycle complete.

The process can stop after any step. Each stopping point leaves a different recovery problem.

If the process stops after step 2, the bank must decide whether to continue or reverse the debit.

If it stops after step 3, the external outcome may be ambiguous.

If it stops after step 4 but before step 5, the business fact may be known even though the local projection is stale.

## Distributed transactions usually stop at the institution boundary

GTB cannot open one ACID transaction that includes its own database, the payment rail, City Bank's ledger, and a settlement system.

That means consistency must be achieved over time using protocols, durable records, retries, idempotency, reconciliation, and exception handling.

This is not an implementation defect. It follows from organizational and trust boundaries.

## Recovery begins with evidence

When a process restarts, it should be able to determine what has already happened.

Useful evidence includes:

- durable local state;
- idempotency keys;
- ledger posting references;
- outbound message IDs;
- external acknowledgements;
- status-query results;
- reconciliation files or reports.

A recovery procedure that depends only on in-memory call stacks is not a recovery procedure.

## Compensation is not rollback

In a database transaction, rollback can make it appear as though an operation never happened.

In payments, an action that escaped the bank's boundary may not be erasable. If a debit was posted and an external payment succeeded, the corrective action may need to be a new business event such as a reversal or return.

This is **compensation**, not time travel.

Compensating actions need their own audit trail and business rules.

## Architecture implication

Design each step by asking:

- Can it be retried safely?
- How can we determine whether it already happened?
- What evidence is durably stored?
- What is the recovery action after a crash?
- If the outcome is unknown, can we query the external system?
- If we cannot know automatically, how does the payment enter investigation?

The result is often a state machine or workflow where uncertainty is explicit.

## Key takeaway

Payment failures are difficult because parts of the business process can succeed independently.

> **A timeout is not failure. A crash is not rollback. Design recovery around durable evidence of what actually happened.**
