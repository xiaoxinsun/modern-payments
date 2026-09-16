# Chapter 4 — Why There Are Different Payment Rails

## Business question

Why does a bank need multiple ways to move money instead of one universal payment network?

Because payments optimize for different business outcomes. A payroll file, an instant retail payment, a multimillion-dollar interbank transfer, and a cross-border supplier payment do not have the same needs for speed, cost, liquidity, availability, finality, reach, or information content.

A **payment rail** is best understood as a combination of rules, participants, messaging, clearing, and settlement arrangements designed for a particular set of needs.

## Scenario: four payments, four priorities

GTB receives four instructions:

1. Maya transfers SGD 50 to another GTB customer.
2. Maya pays a friend at another local bank and expects the result within seconds.
3. GTB's treasury team sends a very high-value interbank payment.
4. A corporate customer pays an overseas supplier in another currency.

All four are “payments.” Treating them as architecturally identical would hide important business differences.

## On-us transfers

If both accounts are maintained by GTB, no external rail is necessary for the value transfer itself. GTB can perform the relevant internal accounting and apply its own product rules.

This path can be fast and operationally simple, but the bank still needs controls, auditability, idempotency, and clear customer status.

On-us payments provide a useful baseline because they show which complexity is intrinsic to banking and which is introduced by interbank coordination.

## Batch or deferred retail payments

Many retail payment systems historically optimize for large numbers of relatively low-value instructions processed in files or cycles. Participants exchange payment information, and obligations may be netted before settlement.

The business advantage is efficiency. The trade-off is that individual payments may not settle immediately.

Architecturally, batch rails create concerns such as cut-off times, file validation, partial rejection, batch identifiers, end-of-cycle settlement, and later returns.

## Instant retail payments

Instant-payment systems aim to give customers rapid availability and near-continuous service. A payer can initiate a transfer and expect a result quickly, often at any time of day.

The faster customer experience shifts pressure onto architecture. Fraud decisions must be fast. Dependencies must be highly available. Timeout handling becomes critical because a few seconds of uncertainty are visible to the customer. Operational tooling must work outside traditional banking hours.

A fast rail does not eliminate settlement and reconciliation. It compresses the time available to handle them safely.

## High-value / RTGS payments

High-value payment systems often prioritize settlement finality, liquidity management, and systemic risk over consumer-style convenience. In a real-time gross settlement model, payments are settled individually rather than netted into a later batch obligation.

For a bank, this means the payment platform may need to understand intraday liquidity, queues, priority, settlement-account balances, and operational controls that do not appear in an ordinary retail transfer.

A payment worth millions should not be modelled as merely a larger version of a fifty-dollar payment.

## Cross-border correspondent payments

Cross-border payments may involve different currencies, jurisdictions, correspondent relationships, time zones, compliance requirements, and local rails.

The end-to-end payment can be a chain of linked obligations rather than one direct transfer between the originating and beneficiary banks.

This architecture often involves richer payment messages, correspondent account management, foreign-exchange decisions, tracking, and exception handling.

## Choosing a rail is a business-routing decision

When GTB receives a payment request, routing may depend on more than destination account details.

The decision can include:

- amount;
- currency;
- urgency;
- beneficiary bank reachability;
- customer product;
- cost;
- operating hours;
- regulatory constraints;
- settlement liquidity;
- scheme limits;
- requested service level.

This is why “payment routing” often deserves its own domain logic rather than being buried inside an adapter.

## Architecture implication

A payment platform should avoid hard-coding the entire product around one rail. Instead, it can separate:

- a canonical internal payment intent;
- eligibility and routing logic;
- rail-specific adapters;
- rail-specific status interpretation;
- common accounting and audit capabilities;
- scheme-specific exception handling.

The goal is not to erase differences between rails. The goal is to isolate them so that common capabilities remain reusable while important scheme semantics remain visible.

## Key takeaway

Different payment rails exist because they solve different business problems.

> **Speed, cost, liquidity, reach, finality, and operating model are business trade-offs first. The architecture follows from those trade-offs.**
