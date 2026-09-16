# Chapter 1 — A Payment Is More Than Moving Money

## Business question

When a customer taps **Send**, what is the bank actually being asked to do?

The customer-facing answer is simple: move money from one account to another. For an engineer, that answer is too vague. A payment is better understood as a **business process that changes financial positions between parties**. Doing that can require the bank to accept an instruction, decide whether it is allowed, record accounting changes, communicate with other institutions, settle obligations, and present a useful status to the customer.

Those activities are related, but they are not the same thing.

## Scenario: Maya pays Daniel

Maya has an account at the fictional **Global Trust Bank (GTB)**. She wants to send SGD 100 to Daniel.

She enters Daniel's details in the GTB mobile app and presses **Send**.

From Maya's point of view, the requirement is clear:

> Take SGD 100 from me and give it to Daniel.

From GTB's point of view, questions immediately appear. Is Maya allowed to use the account? Does she have enough available funds? Is the payment within her limits? Is Daniel also at GTB? What accounting entries are required? Does GTB need to contact an external payment system? What if GTB sends the payment but never receives a response? When can Maya be told that the payment is complete?

These are not exotic edge cases. They are normal payment-processing concerns hidden behind a simple user action.

## An instruction is not the completed payment

When Maya presses Send, GTB first receives an **instruction**. Conceptually:

```text
Transfer SGD 100 from Maya to Daniel.
```

Receiving this instruction does not mean that SGD 100 has already moved. GTB may still need to authenticate Maya, validate the destination, check funds, enforce limits, run required controls, and determine how the payment should be executed.

The first mental model is therefore:

> **Instruction is not completion.**

A payment platform needs to represent both what the customer requested and what subsequently happened.

For example:

- Maya requested SGD 100 to Daniel.
- GTB accepted the request.
- Maya's account was debited.
- An external transfer was submitted.
- Another institution accepted or rejected it.
- Interbank settlement occurred where required.
- Daniel's institution credited Daniel.

Different payment schemes arrange these activities differently. Some combine several steps. Some separate them by seconds, hours, or longer. The important point is that they describe different business facts.

## Messages are not money

Engineers naturally see network calls. GTB sends a request to another system and receives a response. It is easy to imagine that the money somehow travelled inside those messages.

It did not.

Messages communicate instructions, acknowledgements, statuses, and financial information. The financial result comes from institutions changing their records and, where required, settling obligations between themselves.

A useful rule for the rest of the book is:

> **Payment messages describe what institutions should do or have done. They are not themselves the value being transferred.**

This distinction becomes especially important in cross-border payments, where messaging, correspondent account entries, foreign-exchange activity, and settlement can involve different organizations and systems.

## On-us versus external payments

Suppose Daniel also banks with GTB. GTB controls both customer accounts. If the payment is valid, GTB can record accounting changes conceptually like this:

```text
Maya account      - SGD 100
Daniel account    + SGD 100
```

No other bank needs to receive value from GTB merely because Maya paid Daniel. This is commonly called an **on-us** or **book transfer**.

Now suppose Daniel banks with City Bank. GTB can debit Maya's account, but it cannot directly change Daniel's account because City Bank owns that accounting record. A mechanism is needed to move or settle value between the institutions, and City Bank must eventually make its own customer-account entry.

This gives us another foundational distinction:

> **Customer-account accounting and interbank settlement are separate concerns.**

## Where is the truth?

Engineers often ask for a single source of truth for the payment. That phrase is useful only when the scope of the truth is precise.

GTB's customer ledger may be authoritative for Maya's booked balance. GTB's payment system may be authoritative for the instruction and its internal lifecycle. An external payment network may be authoritative for its own processing state. A settlement system may be authoritative for whether an interbank obligation has settled. Daniel's bank is authoritative for Daniel's account.

These facts must be consistent where they overlap, but they are not interchangeable.

That is why a single field such as this is dangerous:

```text
status = SUCCESS
```

What succeeded? Acceptance? Debit? External submission? Settlement? Beneficiary credit? Finality under the scheme rules?

Good payment architecture asks a more precise question:

> **What exactly has become true, in which system, for which participant?**

## The ledger matters, but it is not the whole payment

A bank must maintain authoritative accounting records for the financial positions it is responsible for. If GTB says Maya has SGD 1,000 and processes a SGD 100 debit, the accounting system must correctly represent the resulting position.

A workflow engine cannot substitute for that. Neither can a message queue or a generic payment-status table.

The ledger answers accounting questions. The payment platform answers payment-processing questions. They interact closely but are not the same responsibility.

Suppose GTB's workflow says `SUCCESS`, but the accounting entry was never committed. Or suppose the ledger contains the debit, but the payment service crashed before recording the next processing state. Those are not merely database inconsistencies. They are financial-control problems.

## Several kinds of state

A single payment may create several categories of information:

- **Business facts** — Maya requested the payment; GTB accepted it; the beneficiary was credited.
- **Accounting state** — customer balances, settlement-account movements, fees, receivables, and payables.
- **Processing state** — validation complete, waiting for external response, retry required, investigation required.
- **Customer-visible state** — pending, completed, failed, returned.
- **Operational information** — latency, traces, error counts, queue depth, dependency health.

These categories can be stored in the same physical platform, but they should remain conceptually distinct.

A successful trace is not proof that the accounting is correct. A workflow step is not a ledger entry. A ledger entry is not necessarily a good explanation to a customer.

## Architecture implication

Before drawing services and queues, identify at least these boundaries:

- What records the customer's instruction?
- What system owns the accounting truth?
- What coordinates the payment process?
- Which external institutions or infrastructures participate?
- What records external outcomes?
- What does the customer see?
- How will disagreement between those records be detected?

A diagram that cannot answer these questions is probably hiding important payment semantics.

## Key takeaway

A payment is not merely an API request or database update. It is a business process through which financial positions change and obligations are discharged.

The central mental model for this book is:

> **Do not ask only, “Did the payment succeed?” Ask, “What exactly has become true, in which system, for which participant?”**
