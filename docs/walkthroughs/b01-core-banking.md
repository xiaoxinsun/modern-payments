# Walkthrough: Core Banking accounts, holds and postings (B01)

Core Banking is GTB's ledger, the only authority for account balances. Other
services, payments included, never touch its tables; they call its HTTP port.
In this walkthrough you call that port directly, as the payment service will from
B06 onward.

Prerequisites: [shared setup](README.md) sections 1–3. Only terminal A (Core
Banking) needs to be running.

## Concepts in five minutes

- **Booked vs. available balance.** Booked reflects posted entries. Available is
  booked minus active **holds** (reservations). Only available is computed; it is
  never stored.
- **Holds** reserve funds so the same money can't be spent twice. A hold ends
  either `CONSUMED` (by a posting) or `RELEASED`, exactly once. There is no
  overdraft.
- **Postings** are balanced double-entry journals: total debits equal total
  credits in each currency. Posted entries are never edited. A mistake is
  corrected by a new **reversal** posting that links to the original.
- **Operation IDs.** Every mutation carries a caller-chosen UUID. Repeating it with
  the same payload replays the original result; a different payload returns `409`.
  After a timeout, look the operation up instead of inventing a new ID. This is
  how GTB survives lost replies without double-posting.
- Design rationale: [ADR 0008](../../spec/adr/0008-core-banking-posting-engine.md).
  Contract: `contracts/openapi/v1/core-banking.json`. Accounting scenarios A–E:
  foundation Section 5.

## Code map

| What | Where |
| --- | --- |
| Process wiring: config, auth, routes | `backend/cmd/core-banking-service/main.go` |
| HTTP handlers and error mapping | `backend/internal/corebanking/http.go` |
| Transactions, locking, idempotency, balances | `backend/internal/corebanking/service.go` |
| Pure accounting rules (balancing, sides) | `backend/internal/corebanking/domain/model.go` |
| SQL and generated Go (sqlc) | `backend/internal/corebanking/storage/queries.sql`, `storage/coredb/` |
| Tables and database guards | `backend/migrations/corebanking/0002_*.sql`, `0003_*.sql` |

## Step 1: get a service token and look at Maya

```sh
CB=http://localhost:8084/integrations/v1/core-banking
MAYA=10000000-0000-4000-8000-000000000001
DANIEL=10000000-0000-4000-8000-000000000002
SERVICE=$(service_token payment-service PAYMENT_SERVICE_OIDC_SECRET)

curl -s $CB/accounts/$MAYA -H "Authorization: Bearer $SERVICE" | jq .
```

Expected: `"bookedMinor":"100000","availableMinor":"100000"`, kind
`CUSTOMER_DEPOSIT`. Try the same call with a customer token
(`user_token customer.demo GTB_CUSTOMER_PASSWORD`). It gets `401`, because that
token's audience is `payment-api`: customers can't reach the ledger directly.

## Step 2: reserve funds, replay, conflict

```sh
RESERVE_OP=$(newid)
RESERVE='{"operationId":"'$RESERVE_OP'","accountId":"'$MAYA'","amount":{"amountMinor":"5000","currency":"SGD"}}'

HOLD_ID=$(curl -s $CB/holds -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d "$RESERVE" | tee /dev/stderr | jq -r .holdId)

curl -s $CB/accounts/$MAYA -H "Authorization: Bearer $SERVICE" | jq '{bookedMinor, availableMinor}'
# {"bookedMinor":"100000","availableMinor":"95000"}   <- booked unchanged, available reduced

# Same operation ID and payload: the same hold comes back, not a second one.
curl -s $CB/holds -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' -d "$RESERVE" | jq -r .holdId

# Same operation ID, different amount: 409.
curl -s $CB/holds -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d "${RESERVE/5000/6000}" | jq .error

# Lost-reply recovery: ask what happened to an operation ID.
curl -s $CB/operations/$RESERVE_OP -H "Authorization: Bearer $SERVICE" | jq .
```

Also try reserving more than the available balance, for example
`"amountMinor":"999999"`. You get `409` "insufficient available funds", and
nothing is written.

## Step 3: release the hold

```sh
curl -s $CB/holds/$HOLD_ID/releases -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d '{"operationId":"'$(newid)'"}' | jq '{status, releasedByOperationId}'
# {"status":"RELEASED", ...}; available is back to 100000
```

Releasing it again with a *new* operation ID returns `409`: a hold ends only once.

## Step 4: post a book transfer by consuming a hold, then reverse it

This is foundation Scenario D (reserve, then consume and post atomically),
followed by a Scenario B-style compensation. Balances end where they started, but
both postings stay in history for good.

```sh
HOLD_ID=$(curl -s $CB/holds -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d '{"operationId":"'$(newid)'","accountId":"'$MAYA'","amount":{"amountMinor":"1000","currency":"SGD"}}' | jq -r .holdId)

POSTING_ID=$(curl -s $CB/postings -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' -d '{
  "operationId":"'$(newid)'","holdId":"'$HOLD_ID'","description":"Walkthrough book transfer",
  "entries":[
    {"accountId":"'$MAYA'","side":"DEBIT","amount":{"amountMinor":"1000","currency":"SGD"}},
    {"accountId":"'$DANIEL'","side":"CREDIT","amount":{"amountMinor":"1000","currency":"SGD"}}]}' \
  | tee /dev/stderr | jq -r .postingId)

for a in $MAYA $DANIEL; do curl -s $CB/accounts/$a -H "Authorization: Bearer $SERVICE" | jq -c '{externalReference, bookedMinor, availableMinor}'; done
# Maya 99000/99000, Daniel 21000/21000; the hold was consumed, so available didn't drop twice

curl -s $CB/reversals -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d '{"operationId":"'$(newid)'","originalPostingId":"'$POSTING_ID'","description":"Walkthrough reversal"}' \
  | jq '{postingId, reversalOfPostingId}'

curl -s "$CB/accounts/$MAYA/statement?limit=20" -H "Authorization: Bearer $SERVICE" \
  | jq -c '.entries[] | {description, side, amount: .amount.amountMinor}'
```

Things to try:
- An unbalanced posting (debit 1000, credit 999) returns `400`.
- Reversing the same posting twice returns `409`.
- Reversing a reversal returns `409`.

## Step 5: look at the ledger tables

```sh
gtbsql -d corebanking
```

```sql
\pset pager off
SELECT external_reference, account_kind, booked_balance_minor FROM app.accounts
  WHERE account_id::text LIKE '10000000-0000-4000-8000-%' ORDER BY account_id;   -- the fixtures only
SELECT operation_type, resource_type, committed_at FROM app.operations ORDER BY committed_at DESC LIMIT 6;
SELECT hold_id, amount_minor, status, consumed_by_operation_id IS NOT NULL AS consumed FROM app.holds ORDER BY created_at DESC LIMIT 3;
SELECT j.description, j.reversal_of_posting_id IS NOT NULL AS is_reversal, e.line_number, e.side, e.amount_minor
  FROM app.posting_journals j JOIN app.posting_entries e USING (posting_id)
  ORDER BY j.committed_at DESC, e.line_number LIMIT 6;
SELECT event_type, aggregate_id, payload->>'operationId' AS operation FROM app.posting_events ORDER BY recorded_at DESC LIMIT 3;
SELECT event_id, published_at FROM app.posting_outbox ORDER BY occurred_at DESC LIMIT 3;   -- set by the built-in relay (B03) once Kafka acknowledges
```

Every posting appended one `CoreBankingPostingCommitted` event and one outbox
row in the same transaction. The guards are enforced by the database itself:

```sql
BEGIN;
UPDATE app.posting_entries SET amount_minor = 1;     -- ERROR: posting_entries is append-only
ROLLBACK;
```

## Where this goes next

The payment service will reserve, consume and release through exactly these
calls once workflows arrive (B06), persisting each operation ID *before* calling.
In B02 the payment API only reads accounts, to check who owns them.
