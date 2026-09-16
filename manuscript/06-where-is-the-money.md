# Chapter 6 — Where Is the Money?

## Business question

When a payment changes a customer's balance, what exactly is the bank recording, and which system is responsible for that truth?

This is where payments meet accounting. A payment workflow may describe intent and progress, but a bank needs a durable financial record of positions and movements.

## Scenario: Maya starts with SGD 1,000

Suppose Maya's GTB account shows SGD 1,000. She sends SGD 100 to Daniel.

A naive implementation might simply update a column:

```text
balance = 900
```

That tells us the current result, but not enough about how the bank got there. A financial system generally needs a history of postings that can be audited, reversed, reconciled, and aggregated.

Conceptually, GTB wants to record a movement whose accounting effect is explainable later.

## Ledger thinking

A ledger records financial entries according to accounting rules. The implementation can vary, but several principles matter to payment architects:

- financial effects should be durable;
- entries should be attributable to a business event or instruction;
- corrections should preserve history rather than silently rewriting it;
- balances should be reproducible from authoritative records;
- related debit and credit effects should maintain accounting integrity.

The payment service should not casually become a ledger merely because it has a database.

## Booked balance and available balance

Two balances are especially important to distinguish.

**Booked balance** reflects entries that have been posted to the account.

**Available balance** reflects what the customer can currently use, which may also consider holds, reservations, overdraft facilities, or pending items.

Suppose Maya has SGD 1,000 booked, but SGD 200 is reserved for another obligation. A new SGD 900 payment might be invalid even though the booked balance appears sufficient.

This is why “check balance, then later debit” can be unsafe if other transactions can consume the same funds in between.

## Reservation versus posting

Some payment designs reserve funds before final execution. Others post the debit immediately. The right model depends on the product and rail.

A reservation can represent:

> These funds are still part of the account's booked balance, but they are no longer available for other spending because this payment has claimed them.

Later, the reservation may be converted into a final posting or released if the payment does not proceed.

Reservations introduce their own lifecycle: creation, expiry, consumption, release, reconciliation.

## The ledger is a system of record, not a workflow engine

Suppose a payment is waiting three hours for an external response. The ledger does not need to model every retry and timeout. Those are workflow concerns.

Conversely, the workflow engine should not be treated as the authoritative financial record merely because it remembers that `DebitAccount` completed.

A useful separation is:

- **ledger** — what financial positions and entries exist;
- **payment domain** — what the payment means and where it is in its business lifecycle;
- **workflow/orchestration** — what work remains to be performed;
- **projection** — what a customer or operator should see.

## Double-entry as a mental model

Bank accounting is broader than this chapter, but payment engineers should understand the core idea: financial movements are represented through balanced entries rather than unexplained mutation.

An internal transfer may conceptually involve one customer's account decreasing and another increasing. An external payment may also affect internal settlement, suspense, fee, or correspondent accounts depending on the design.

The exact accounting should be owned by finance/accounting domain experts. The architect's responsibility is to preserve the boundary and transactional integrity of those postings.

## Architecture implication

The connection between payment and ledger needs a deliberate consistency strategy.

Questions include:

- Can the payment record and ledger posting commit atomically because they share a database boundary?
- If not, which side is authoritative when one succeeds and the other does not?
- How is a duplicate posting prevented?
- How is a failed posting retried safely?
- How is a payment correlated to all of its accounting entries?
- How are compensating or reversing entries represented?

There is no universal implementation, but there must be a defensible answer.

## Key takeaway

A payment changes financial positions. Those changes belong in a ledger or equivalent authoritative accounting system, not merely in workflow state.

> **Keep payment intent, accounting truth, and processing coordination distinct—even when they live close together technically.**
