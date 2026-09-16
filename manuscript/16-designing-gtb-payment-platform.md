# Chapter 16 — Designing Global Trust Bank's Payment Platform

## Business question

If we started from the business requirements rather than from a technology diagram, what would a modern bank payment platform look like?

This chapter combines the mental models from the rest of the book. It is not intended as the one correct architecture. Its purpose is to show how architecture can be **derived from business facts, responsibility boundaries, and failure modes**.

## GTB's requirements

Global Trust Bank wants one platform that can support:

- on-us account transfers;
- domestic instant payments;
- high-value payments;
- cross-border account-to-account payments;
- customer and operations status views;
- strong auditability;
- reconciliation;
- repair and investigation;
- additional rails in the future.

The platform must not assume that all rails have the same lifecycle or settlement model.

## Scenario: four payments arrive at GTB

During the same minute, GTB receives four instructions: an internal transfer between two GTB customers, an instant domestic payment to another bank, a high-value treasury payment, and a cross-border supplier payment.

The customer-facing action is similar in every case: send value to a beneficiary. The processing obligations are not. This is the design pressure the platform must absorb without pretending that every rail is the same.

## Step 1: define the payment intent

GTB begins with a stable internal concept of a **payment instruction**.

It captures the business request:

- payer account;
- beneficiary details;
- amount and currency;
- requested execution time;
- payment purpose/reference;
- requested service where applicable;
- customer request identity.

This is not yet a rail message and not yet an accounting entry.

## Step 2: establish acceptance

A payment domain service coordinates the decision about whether GTB will accept the instruction.

It consults capabilities that own their own concerns:

```text
Customer / channel
      |
      v
Payment API
      |
      v
Payment domain
  | entitlement
  | funds / limits
  | risk controls
  | route eligibility
      |
      v
Accepted / rejected
```

The acceptance record includes enough evidence to explain the decision later.

## Step 3: keep accounting authoritative

GTB's ledger remains authoritative for customer financial positions and related accounting entries.

The payment domain references ledger postings rather than pretending that its own status field is the balance.

For a simple on-us transfer, the route may stay entirely inside GTB:

```text
Payment domain
      |
      v
Ledger
  debit Maya
  credit Daniel
      |
      v
Payment completed
```

Even here, idempotency and auditability matter.

## Step 4: route external payments deliberately

For an external payment, a routing capability chooses an eligible rail based on amount, currency, destination, product, service level, scheme rules, and operational availability.

```text
Payment domain
      |
      v
Routing decision
   /      |       \
Instant  High    Cross-border
rail     value      rail
```

Routing is a business decision, not simply URL selection.

## Step 5: isolate rail adapters

Each rail adapter owns scheme-specific concerns:

- message formats;
- transport and security;
- rail identifiers;
- acknowledgements;
- status callbacks or polling;
- scheme error codes;
- rail-specific operational metrics.

The payment domain preserves the external evidence and translates it into GTB's own business lifecycle without discarding the original semantics.

## Step 6: orchestrate only where needed

A simple instant transfer may use a short state machine and event-driven interactions.

A cross-border payment with timers, external callbacks, compliance holds, and manual repair may use durable workflow orchestration.

A workflow engine such as Temporal can be introduced for these long-running processes, but it remains a coordinator.

It does not become GTB's ledger or redefine the meaning of payment completion.

## Step 7: publish business facts safely

When GTB records important payment facts, downstream systems may need notifications for customer status, analytics, operations, or reporting.

GTB therefore publishes domain events with a consistency strategy such as a transactional outbox where appropriate.

Consumers build read models rather than writing back into authoritative payment or ledger records casually.

## Step 8: design reconciliation into every external boundary

For each rail, GTB defines:

- what external report or statement will be reconciled;
- which identifiers match records;
- what totals should balance;
- how quickly breaks are detected;
- what workflow resolves them.

The platform also reconciles the payment domain to the ledger so that a “completed” payment without the expected accounting evidence is visible.

## Step 9: give operations a business view

An operations console should not expose only microservice health.

For one payment, it should show:

- customer instruction;
- current business lifecycle;
- ledger references;
- route chosen;
- external identifiers and statuses;
- workflow state;
- reconciliation status;
- audit history;
- allowed repair actions.

This turns operations into controlled business-process management rather than database surgery.

## A conceptual architecture

```text
                +------------------+
Customer -----> | Channels / APIs  |
                +--------+---------+
                         |
                         v
                +------------------+
                | Payment Domain   |
                | intent/lifecycle |
                +--+----+----+-----+
                   |    |    |
          +--------+    |    +----------------+
          v             v                     v
   +-------------+ +-----------+        +-------------+
   | Entitlement | | Risk /    |        | Routing     |
   | Limits etc. | | Controls  |        |             |
   +-------------+ +-----------+        +------+------+ 
                                                 |
                              +------------------+------------------+
                              v                  v                  v
                        +-----------+      +-----------+      +-----------+
                        | Instant   |      | High-value|      | Cross-    |
                        | Adapter   |      | Adapter   |      | border    |
                        +-----------+      +-----------+      +-----------+

                +------------------+
                | Ledger           |
                | accounting truth |
                +------------------+

                +------------------+
                | Workflow /       |
                | Orchestration    |
                +------------------+

                +------------------+
                | Events / Read    |
                | Models           |
                +------------------+

                +------------------+
                | Reconciliation & |
                | Operations       |
                +------------------+
```

This diagram intentionally separates responsibilities. A real deployment may combine several boxes or split one box into many services.

## What should not be centralized blindly

A “unified payments platform” can become dangerous if unification means pretending that all rails and states are identical.

GTB should centralize where the business concepts genuinely match:

- customer payment intent;
- common acceptance controls;
- audit conventions;
- identifier standards;
- observability conventions;
- reconciliation framework;
- operations experience.

It should preserve variation where the business differs:

- rail message semantics;
- settlement model;
- finality rules;
- cut-offs;
- return procedures;
- scheme limits;
- status codes.

## Architecture implication

The platform architecture is a consequence of explicit ownership boundaries. Payment intent and lifecycle belong to the payment domain; financial positions belong to the ledger; scheme behavior belongs at rail boundaries; long-running coordination belongs in orchestration where necessary; customer and operations views are projections; and reconciliation independently tests that the records still agree.

This also gives GTB a practical rule for service decomposition: split responsibilities when they have different authority, lifecycle, scaling, failure, or control requirements—not merely to increase the number of services.

## Design exercise

Take a payment from Maya at GTB to Daniel at another bank and answer these questions:

1. What is the customer's instruction?
2. Which system records acceptance?
3. Which system owns Maya's balance?
4. What identifier prevents duplicate submission?
5. Which route is selected and why?
6. What external evidence proves the rail accepted the payment?
7. What does GTB mean by `Completed`?
8. How does GTB know settlement and accounting records agree?
9. What happens after a timeout?
10. What can operations safely repair?

If the architecture cannot answer these questions, the design is not yet complete.

## Final mental model

A payment platform is not primarily a set of APIs. It is a controlled mechanism for creating and proving financial facts across boundaries that do not share one transaction.

The essential architecture follows from five questions:

1. **What business fact are we trying to establish?**
2. **Which system or institution is authoritative for that fact?**
3. **What financial entry represents it?**
4. **How do we proceed safely when the next step is uncertain?**
5. **How will we later prove that all relevant records agree?**

## Key takeaway

The best payment architecture begins with business meaning and failure consequences, not with microservices, queues, or workflow engines.

> **Business facts define the boundaries. Accounting defines the financial truth. Distributed-systems patterns make the process survivable. Reconciliation proves that the pieces still agree.**
