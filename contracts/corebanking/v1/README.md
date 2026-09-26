# Core Banking port v1

The normative contract is [`openapi/v1/core-banking.json`](../../openapi/v1/core-banking.json);
design rationale is in ADR 0008. This page summarizes the semantics.

The internal port is rooted at `/integrations/v1/core-banking` and requires a
bearer token for the `core-banking-service` audience with `internal_service`.
All command identifiers are caller-supplied canonical UUIDv4 operation IDs.
Repeating an operation ID with the same canonical command returns its original
hold or posting; reuse with different financial fields returns HTTP 409.

| Method and path | Purpose |
| --- | --- |
| `POST /holds` | Atomically reserve available customer funds. |
| `GET /holds/{holdId}` | Inspect reservation state. |
| `POST /holds/{holdId}/releases` | Explicitly release an active hold. |
| `POST /postings` | Commit balanced entries, optionally consuming one hold. |
| `POST /reversals` | Append an exact opposite posting linked to an original. |
| `GET /operations/{operationId}` | Recover the durable result after a lost reply. |
| `GET /accounts/{accountId}` | Return booked and reservation-adjusted balances. |
| `GET /accounts/{accountId}/statement` | Export immutable posting lines; optional RFC3339 `from`, `to`, and limit 1–500. |

Money uses the shared `{amountMinor, currency}` contract. Posting commands carry
two to 64 entries with `DEBIT` or `CREDIT`; debits and credits must balance in
each currency. The service rejects cross-currency accounts, negative resulting
balances, expired holds, double consumption, and blind reversal reuse.

Successful posting transactions atomically persist the operation, immutable
journal and entries, balance changes, `CoreBankingPostingCommitted` journal
event, and unpublished outbox row. B03 owns publication and delivery state.
