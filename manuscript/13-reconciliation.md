# Chapter 13 — How Do We Know the Money Is Right?

## Business question

After millions of payment operations, how does the bank prove that its records agree with the outside world and with its own accounting?

The answer is **reconciliation**.

Reconciliation is sometimes treated as back-office cleanup. In a payment platform, it is a core correctness mechanism. Distributed systems can experience lost responses, delayed files, duplicate messages, manual adjustments, and software defects. Reconciliation detects when the resulting records disagree.

## Scenario: everything looks successful

GTB's payment dashboard says 100,000 outbound payments completed today.

The ledger shows corresponding customer debits.

The external rail's settlement report reflects 99,999 matching items.

One payment is missing.

Without reconciliation, GTB may discover the mismatch only when a customer complains or a counterparty raises an investigation.

## Reconciliation compares independent evidence

The strongest reconciliations compare records produced by different systems or parties.

Examples include:

- payment platform vs ledger;
- outbound instructions vs rail acknowledgements;
- rail transaction records vs settlement reports;
- internal settlement accounts vs external statements;
- beneficiary-credit confirmations vs original instructions.

If two reports are generated from the same corrupted table, agreement between them proves little.

## Matching

Reconciliation begins by matching records using identifiers and business attributes.

A match might use:

- payment ID;
- external transaction reference;
- amount and currency;
- value date;
- participant;
- message ID;
- batch or settlement reference.

Good identifier design in earlier chapters pays off here. Poor identifiers turn reconciliation into guesswork.

## Breaks

A **break** is a mismatch that needs explanation.

Examples:

- internal record exists, external record missing;
- external record exists, internal record missing;
- amount differs;
- duplicate external record;
- settlement total does not equal expected obligation;
- lifecycle says complete but accounting entry is absent.

Not every break means money is lost. It means the evidence does not yet agree.

## Continuous and periodic reconciliation

Traditional systems may reconcile at end of day or after a settlement cycle. Modern platforms can also perform near-real-time checks.

The two approaches complement each other. Continuous reconciliation detects issues quickly. Periodic reconciliation can compare authoritative end-of-cycle reports and totals.

The business risk should determine the frequency.

## Repair must preserve evidence

When a break is resolved, the platform should record what changed and why.

Avoid silently editing records until two systems happen to match. The repair itself is a business/operational fact.

A correction may involve:

- posting a missing entry;
- sending a missing instruction;
- suppressing a duplicate;
- creating a reversal;
- correcting a projection;
- opening an investigation.

Each should be auditable.

## Architecture implication

Reconciliation should be designed from the beginning, not bolted on later.

For each critical boundary, define:

1. What two sources will be compared?
2. What identifier matches them?
3. What totals should balance?
4. How quickly should a mismatch be detected?
5. What workflow handles the break?
6. Which corrections are automatic and which require approval?

This turns reconciliation into an explicit control system.

## Key takeaway

A payment platform cannot rely solely on transactional code to prove correctness across independent systems.

> **Reconciliation is how the bank continuously tests whether its view of money agrees with other authoritative evidence.**
