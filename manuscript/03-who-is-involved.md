# Chapter 3 — Who Is Actually Involved?

## Business question

Who must cooperate for a payment to happen, and which responsibilities belong to each participant?

Many architectural misunderstandings begin because a diagram shows only “sender service → payment network → receiver service.” Real payment ecosystems are organized around institutions, accounts, obligations, and rules. The software matters, but the business roles come first.

## Scenario: Maya pays Daniel at another bank

Maya is the person initiating the payment. Daniel is the intended beneficiary. GTB holds Maya's account. City Bank holds Daniel's account.

Between GTB and City Bank may sit one or more infrastructures that provide messaging, clearing, routing, settlement, or scheme governance. A central bank or commercial settlement bank may provide the accounts in which interbank obligations are settled. Other intermediaries may appear in cross-border scenarios.

Each party sees only part of the end-to-end story.

## Customer roles

Payment terminology varies across schemes and standards, but two business roles are universal:

- the party whose funds are used to make the payment;
- the party intended to receive the value.

Standards may call them payer/payee, debtor/creditor, originator/beneficiary, or use more specific terms. Engineers should learn the terminology used by the target rail, but keep the economic roles clear.

## Account-servicing institutions

GTB and City Bank are not just transport endpoints. They maintain customer relationships and accounting records.

The sending institution may be responsible for authenticating the customer, applying limits and controls, checking account state, recording the debit, constructing the payment message, and responding to later exceptions.

The receiving institution may validate the inbound payment, apply its own controls, record the beneficiary credit, notify the beneficiary, and handle later returns or investigations.

A useful architecture boundary follows naturally: **the bank can directly control only the state it owns**. It cannot atomically commit a transaction across another bank's ledger.

## Payment schemes

A payment scheme defines rules for participation and processing. It may specify message formats, operating hours, eligibility, error codes, settlement arrangements, dispute procedures, and service-level expectations.

A scheme is therefore more than a network protocol. It is a shared business contract among participants.

When an engineer says “the API allows this,” the payments engineer must also ask: **do the scheme rules allow it, and what obligations follow?**

## Clearing and settlement infrastructure

Some infrastructures route instructions and calculate obligations. Others provide settlement. Some combine several functions.

For architecture purposes, separate the capabilities conceptually:

- **routing/message exchange** — getting payment information to the right participant;
- **clearing** — validating/exchanging instructions and possibly calculating obligations;
- **settlement** — discharging obligations between participants;
- **scheme governance** — defining the operating rules.

A single real-world organization may provide more than one of these.

## Settlement institutions

When two banks owe one another money, they need a settlement mechanism. In many domestic systems, settlement ultimately occurs across accounts held at a central bank or another designated settlement institution.

The important architectural insight is not the exact institution. It is that **customer money on a bank ledger and settlement assets between banks live in different accounting domains**.

That means GTB may need to manage both customer-account postings and positions in settlement or correspondent accounts.

## Intermediaries in cross-border payments

Cross-border payments can add correspondent banks, foreign-exchange providers, local clearing systems, sanctions controls, and different operating hours.

Suppose GTB does not hold a direct account relationship with Daniel's overseas bank. GTB may use a correspondent institution that maintains the relevant currency account. The payment can therefore involve several banks changing balances on accounts they maintain for one another.

This is why “send the SWIFT message” is not equivalent to “move the money.” Messaging may coordinate changes in a chain of institutional accounts.

## Responsibility boundaries matter more than service boundaries

A microservice diagram may show twenty services inside GTB. From the business perspective, they still belong to one institution.

By contrast, the boundary between GTB and City Bank is fundamentally different. GTB cannot rely on a distributed transaction across both institutions. It must use messages, identifiers, acknowledgements, reconciliation, and scheme procedures.

This suggests a useful architectural rule:

> **Draw institutional trust boundaries before drawing internal service boundaries.**

The most consequential failure modes often occur where responsibility crosses institutions.

## Architecture implication

For each participant, ask:

- What business facts can it assert authoritatively?
- Which accounts or ledgers does it own?
- Which rules constrain it?
- Which messages can it send or receive?
- What happens when another participant disagrees?
- How are obligations reconciled and settled?

This produces clearer interfaces than starting from endpoint names.

## Key takeaway

A payment ecosystem is a network of **responsibilities and accounting relationships**, not just a network of computers.

> **Understand who owns each obligation and each financial record before deciding how the software should interact.**
