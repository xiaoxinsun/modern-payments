# Verification Ledger

This first draft intentionally avoids many detailed real-world claims. Before publication, factual descriptions that can vary by jurisdiction, scheme version, or product release should be checked against primary sources.

## Payment-system facts to verify

- Definitions and legal treatment of **settlement finality** in each jurisdiction used as an example.
- Current operating and settlement model of Singapore **FAST**.
- Current operating and settlement model of Singapore **MEPS+**.
- Any quantitative claims about availability, limits, cut-offs, settlement timing, or processing speed of named payment infrastructures.
- Current relationships among clearing, settlement, and central-bank money for any named rail.

## SWIFT and cross-border

- Precise current role of SWIFT in financial messaging versus settlement.
- Current SWIFT/ISO 20022 adoption status and message types if discussed in detail.
- Any specific correspondent-banking flow or named service.

## ISO 20022

- Current message definitions and terminology used in examples.
- Scheme-specific implementation guidelines; avoid implying that base ISO 20022 alone defines a rail's complete business behavior.

## Regulation and retention

- Audit-log retention requirements.
- Payment-record retention requirements.
- Regulatory screening obligations.
- Consumer cancellation, return, refund, or error-resolution rights.

These are jurisdiction-specific and should not be generalized from one market.

## Workflow technology

If Temporal or another workflow product is described beyond the conceptual level, verify against current product documentation:

- replay/determinism model;
- workflow/activity semantics;
- retry and timer behavior;
- versioning/deployment guidance;
- durability and persistence guarantees;
- operational limits and performance claims.

## Editorial verification rule

For every real infrastructure or regulation added in later drafts:

1. prefer primary sources from the operator, regulator, central bank, or standards body;
2. record the publication/version date;
3. distinguish scheme rules from common industry practice;
4. distinguish factual claims from GTB's fictional design choices.
