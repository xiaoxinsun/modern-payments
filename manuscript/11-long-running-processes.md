# Chapter 11 — Long-Running Business Processes

## Business question

What changes when a payment process lasts longer than a single request, server process, or deployment?

Some payments finish in seconds. Others wait for cut-off times, external acknowledgements, investigations, settlement windows, manual review, or returns. A bank needs to preserve the process even when individual application instances disappear.

## Scenario: the payment that lasts two days

A corporate customer asks GTB to make an international supplier payment.

GTB validates and accepts the instruction today. A compliance review pauses it. The payment is released later. GTB sends it to a correspondent. The correspondent requests clarification. Operations supply the information the next morning. The payment proceeds and is eventually confirmed.

No single HTTP request should remain open for this entire period.

## Durable workflow state

A long-running process needs to remember:

- what has already happened;
- what it is waiting for;
- what should happen next;
- which retries have occurred;
- what deadlines or timers matter;
- whether manual intervention is required.

This is **workflow state**. It is different from the payment's business facts and different again from accounting state.

## Orchestration versus choreography

In choreography, services react to events without one component explicitly controlling the end-to-end sequence.

In orchestration, a workflow component maintains the process and decides which step comes next.

Both are useful. A simple payment flow may work well with event-driven choreography. A long process with timers, retries, branching, and human steps often benefits from explicit orchestration because the business sequence becomes easier to reason about.

The decision should follow process complexity, not technology fashion.

## Why durable execution is attractive

A durable workflow engine can make code resemble a normal sequential process while the platform persists enough state to survive worker crashes and restarts.

This can reduce custom code for:

- retries;
- timers;
- waiting for callbacks;
- resuming after failures;
- long-lived state machines.

Technologies such as Temporal are relevant here because they address this class of problem. They do not replace the payment domain model, ledger, message broker, or external rail.

## Determinism and replay

Some workflow systems reconstruct state by replaying a recorded history. That can impose constraints on workflow code: decisions must replay consistently, while external side effects are isolated in activities or equivalent units.

For a payment architect, the key idea is that the workflow history is evidence about orchestration progress, not the bank's accounting ledger.

A workflow can remember that `PostDebit` completed. The accounting system remains authoritative for whether the debit exists.

## Human intervention is part of the workflow

Payment operations frequently need repair queues, investigations, approvals, or data enrichment.

A robust workflow should be able to enter a state such as `REQUIRES_REVIEW` without losing the surrounding context. After an operator acts, the process should resume from a well-defined point.

This is better than asking operations to manually reconstruct what the system was trying to do from logs.

## Architecture implication

Use orchestration when the business process justifies it:

- many dependent steps;
- long waits;
- timers or deadlines;
- external callbacks;
- repeated retries;
- human tasks;
- complex compensation;
- strong need for operational visibility.

Avoid putting everything into one giant workflow. Payment lifecycle, accounting, rail adapters, and domain services should remain separately understandable.

## Key takeaway

Long-running payments need durable coordination, but workflow state is not financial truth.

> **Use orchestration to remember what work remains. Use domain records and ledgers to record what business and financial facts are true.**
