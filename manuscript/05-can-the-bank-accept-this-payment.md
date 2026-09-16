# Chapter 5 — Can the Bank Accept This Payment?

## Business question

Before a bank moves money, how does it decide whether the requested payment is allowed to proceed?

A payment instruction is not automatically executable. The bank must establish that the customer, account, amount, destination, and requested service satisfy a set of business, risk, operational, and regulatory conditions.

## Scenario: a payment that looks simple

Maya asks GTB to send SGD 8,000 to a new beneficiary.

The app has already authenticated her. That does not answer every relevant question.

GTB may still need to ask:

- Is Maya entitled to debit this account?
- Is the account open and able to make this type of payment?
- Are sufficient available funds present?
- Does the amount exceed a product or customer limit?
- Is the destination valid and reachable on the chosen rail?
- Does the payment trigger fraud or financial-crime review?
- Is the requested execution time supported?

A payment system is therefore partly a **decision system**.

## Authentication and authorization are different

Authentication asks: **who is the user?**

Authorization asks: **may this user perform this action on this account under these conditions?**

A strongly authenticated user might still be unable to debit a corporate account, exceed a transaction limit, or use a particular rail.

Payment architecture should keep these questions separate because they have different evidence, rules, and audit requirements.

## Available funds are not always the booked balance

A customer may see a booked balance of SGD 10,000 while only SGD 7,000 is available to spend because of holds, pending card transactions, overdraft rules, or reserved amounts.

The payment platform should therefore avoid making its own informal balance calculation unless the bank's accounting design explicitly assigns that responsibility.

The safe design question is:

> Which system is authoritative for whether these funds are available for this payment now?

## Limits are business policy

Banks commonly apply limits at several levels: transaction, daily, customer, account, channel, beneficiary, or scheme.

These limits are not merely validation constants. They may vary by customer segment, product, risk level, and authentication method.

Treating limits as domain policy allows them to evolve without scattering values across application code.

## Fraud and financial-crime controls

A payment can be technically valid and financially funded while still requiring risk checks.

Fraud controls may look for unusual device, beneficiary, amount, or behavioral patterns. Financial-crime controls may require screening or additional review depending on the payment and jurisdiction.

The architecture lesson is not to implement these controls inside the payment service itself. Instead, treat them as explicit decision dependencies with clear outcomes such as approve, reject, or review.

If a control is unavailable, the product must have a defined business policy. “Retry forever” is not a risk strategy.

## Validation should produce explainable outcomes

A rejected payment should carry a reason that is meaningful for downstream handling.

For example:

```text
ACCOUNT_CLOSED
INSUFFICIENT_AVAILABLE_FUNDS
LIMIT_EXCEEDED
BENEFICIARY_INVALID
RAIL_UNAVAILABLE
REVIEW_REQUIRED
```

The exact codes depend on the bank and rail, but the principle is general: a rejection is a business result, not just an exception stack trace.

## Architecture implication

A useful acceptance pipeline separates concerns:

```text
Instruction
   |
   v
Structural validation
   |
   v
Customer/account entitlement
   |
   v
Funds and limits
   |
   v
Risk/compliance decisions
   |
   v
Routing eligibility
   |
   v
Accept or reject
```

Not every payment needs every step, and some can execute in parallel. What matters is that the final acceptance decision is based on recorded evidence.

This creates several engineering requirements:

- decision inputs should be traceable;
- re-running a check should not accidentally duplicate financial effects;
- expensive controls may need caching or carefully scoped reuse;
- results should have explicit validity periods where appropriate;
- rejection reasons should be stable enough for operations and analytics.

## Key takeaway

The bank's first responsibility is not to move money quickly. It is to determine whether it is allowed to move the money at all.

> **Payment acceptance is a business decision composed from identity, entitlement, funds, limits, risk, and rail eligibility.**
