# Walkthrough: payment commands and durable receipts (B02)

B02 builds the front door for payments. A customer submits an instruction, and
GTB **durably records** it: one database transaction writes the instruction, an
idempotency record, a command receipt, an audit entry, a `PaymentReceived` event
and a "please start the workflow" dispatch row. The API answers `202 Accepted`.

`202` here means **received, not accepted or paid**. No funds are reserved and no
workflow starts yet. Those arrive with controls (B05) and the workflow
dispatcher (B06). Maya's balance doesn't change in this walkthrough.

Prerequisites: [shared setup](README.md) sections 1–3, with both services running
(terminal A Core Banking, terminal B payment-api). The [B01 walkthrough](b01-core-banking.md)
is optional background.

## Concepts

- **Idempotency.** Clients send an `Idempotency-Key` header. The duplicate
  identity is *(channel `CUSTOMER_API`, `customerId`, key)*. Same identity and
  same payload returns the original payment (`Idempotent-Replayed: true`); same
  identity with a different payload returns `409`. Keys never expire. A client
  whose request timed out retries with the **same** key and never invents a new
  one.
- **Authorization happens in layers.**
  1. The token must be valid and meant for `payment-api` (otherwise `401`).
  2. The route requires a role: `customer` to submit; `customer` or an operator
     read role to read.
  3. The body's `customerId` must be in the token's `customer_ids`.
  4. Core Banking must confirm that the debtor account belongs to that customer.
     Unknown and foreign accounts are treated the same, to avoid leaking which
     accounts exist.
- **The lifecycle has separate dimensions.** `lifecycleState` (`RECEIVED` → …
  → `COMPLETED`) is independent of `executionOutcome` (`NOT_SUBMITTED`,
  `PENDING`, `UNKNOWN`, …). Every change bumps `version` and appends a history
  row recording its actor, policy version and evidence.
- **Claims.** Cancellation and submission compete for one exclusive claim, and
  whichever commits first wins. B02 builds this primitive; B06 uses it.
- Design rationale: [ADR 0009](../../spec/adr/0009-payment-command-model-and-receipts.md).
  Contract: `contracts/openapi/v1/payment.json`.

## Code tour: follow one `POST /api/v1/payments`

Read these in order; each hands off to the next.

1. `backend/cmd/payment-api/main.go`: the composition root. It loads config,
   opens the database pool, builds the Core Banking client with a
   client-credentials token source, and wires the pieces together. Go has no DI
   container; wiring is plain constructor calls.
2. `backend/internal/platform/runtime/http.go`: middleware for correlation IDs,
   access logs and panic recovery, plus the error envelope.
3. `backend/internal/payment/api/handler.go`:
   - `Register`: authentication and role checks per route.
   - `requestValidator`: validates the request against the OpenAPI contract
     (kin-openapi). `"amountMinor":"0100"` is rejected here.
   - `SubmitPayment`: maps the wire type to the domain type.

   `api.gen.go` is generated from the contract, much like an OpenAPI generator's
   server stubs in Java.
4. `backend/internal/payment/application/service.go`: `Submit` checks customer
   scope, validates the instruction, checks for a replay, and calls Core Banking
   to authorize the accounts.
5. `backend/internal/adapters/corebanking/client.go`: the HTTP call to Core
   Banking. Only a `200` or `404` counts as an answer; anything else becomes a
   retryable `503`.
6. `backend/internal/payment/storage/store.go`: `Receive` runs the single
   transaction (advisory lock on the idempotency scope, then eight inserts).
   `Apply` is the versioned-change primitive for later phases.
7. `backend/internal/payment/domain/`: pure rules with no I/O.
   - `instruction.go`: product validation and the canonical hash.
   - `payment.go`: the lifecycle graph, claims and the execution owner.

Tests read as executable specifications. From `backend/`:

```sh
go test ./internal/payment/domain -run TestCancellationAndSubmissionClaimsAreExclusive -v
go test ./internal/payment/... -v                 # database tests skip without a database URL
make -C .. payment-integration                    # the same, against the real database
go doc ./internal/payment/application Store       # Go's built-in docs for a type
```

## Step 1: tokens and a request body

```sh
PAY=http://localhost:8082/api/v1
CUSTOMER=$(user_token customer.demo GTB_CUSTOMER_PASSWORD)
OPS=$(user_token operations.demo GTB_OPERATIONS_PASSWORD)
COMPLIANCE=$(user_token compliance.demo GTB_COMPLIANCE_PASSWORD)
claims "$CUSTOMER" | jq '{aud, customer_ids, roles: .realm_access.roles}'

KEY="walkthrough-$(date +%s)"
BODY='{
  "customerId": "20000000-0000-4000-8000-000000000001",
  "product": "BOOK_TRANSFER",
  "debtorAccountId": "10000000-0000-4000-8000-000000000001",
  "amount": {"amountMinor": "2500", "currency": "SGD"},
  "creditor": {"name": "Daniel", "accountId": "10000000-0000-4000-8000-000000000002"},
  "reference": "lunch"
}'
```

## Step 2: submit

```sh
curl -s -i $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' -d "$BODY"
```

Expected (IDs and times differ):

```text
HTTP/1.1 202 Accepted
Idempotent-Replayed: false
X-Correlation-Id: 95a3fe02-…
{"commandId":"1f8a4ca0-…","dispatch":{"attempts":0,"status":"PENDING","workflowId":"payment/36ea81b9-…",…},
 "executionOutcome":"NOT_SUBMITTED","lifecycleState":"RECEIVED","paymentId":"36ea81b9-…","receiptVersion":"1",…}
```

Save the IDs for the next steps:

```sh
RECEIPT=$(curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' -d "$BODY")
PAYMENT_ID=$(jq -r .paymentId <<< "$RECEIPT"); COMMAND_ID=$(jq -r .commandId <<< "$RECEIPT")
```

That second call was already a **replay**: it returned the same payment. Check
the header:

```sh
curl -s -D - -o /dev/null $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' -d "$BODY" | grep -i replayed      # Idempotent-Replayed: true
```

## Step 3: conflicting reuse

```sh
curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' -d "${BODY/2500/2600}" | jq .
# 409 {"error":{"code":"CONFLICT","message":"Idempotency-Key was already used with a different payload","retryable":false},…}
```

## Step 4: denied requests

Each of these returns an error envelope and writes nothing. Give each its own key
so that none of them is treated as a replay:

```sh
post() { curl -s -o /dev/null -w "%{http_code}\n" $PAY/payments -H "Authorization: Bearer $1" \
  -H "Idempotency-Key: $(newid)" -H 'Content-Type: application/json' -d "$2"; }

post "$OPS" "$BODY"                                                   # 403: operators cannot submit
post "$CUSTOMER" "$(jq '.debtorAccountId="10000000-0000-4000-8000-000000000002" | .creditor.accountId="10000000-0000-4000-8000-000000000001"' <<< "$BODY")"
                                                                      # 403: Daniel's account under Maya's customerId
post "$CUSTOMER" "$(jq '.customerId="20000000-0000-4000-8000-000000000002"' <<< "$BODY")"   # 403: customer not in token scope
post "$CUSTOMER" "$(jq '.amount.amountMinor="0100"' <<< "$BODY")"    # 400: contract pattern (non-canonical amount)
post "$CUSTOMER" "$(jq '.amount.amountMinor=2500' <<< "$BODY")"      # 400: amounts are strings, never numbers
post "$CUSTOMER" "$(jq '.creditor.accountId="10000000-0000-4000-8000-000000000001"' <<< "$BODY")"  # 400: self-transfer
post "$CUSTOMER" "$(jq '.product="RTGS"' <<< "$BODY")"                # 400: external products need accountNumber/bankIdentifier
post "" "$BODY"                                                       # 401: no token
```

An external product needs an external creditor instead of a GTB account. This one
passes validation and creates a real receipt:

```sh
post "$CUSTOMER" "$(jq '.product="DOMESTIC_INSTANT" | .creditor={"name":"Daniel","accountNumber":"0123-456","bankIdentifier":"OCBCSGSG","country":"SG"}' <<< "$BODY")"   # 202
```

## Step 5: read the receipt and the command

```sh
curl -s $PAY/payments/$PAYMENT_ID/receipt -H "Authorization: Bearer $CUSTOMER" | jq .
curl -s $PAY/commands/$COMMAND_ID -H "Authorization: Bearer $CUSTOMER" | jq .
curl -s -o /dev/null -w "%{http_code}\n" $PAY/payments/$PAYMENT_ID/receipt -H "Authorization: Bearer $OPS"         # 200
curl -s -o /dev/null -w "%{http_code}\n" $PAY/payments/$PAYMENT_ID/receipt -H "Authorization: Bearer $COMPLIANCE"  # 403
```

The receipt shows the immutable `instruction`, `version`, lifecycle and outcome,
and `dispatch.status: PENDING`. These reads come straight from the payment
database. The search and list endpoints are projections, built in B03.

Confirm that Maya's money has not moved, since nothing is reserved yet:

```sh
curl -s http://localhost:8084/integrations/v1/core-banking/accounts/10000000-0000-4000-8000-000000000001 \
  -H "Authorization: Bearer $(service_token payment-service PAYMENT_SERVICE_OIDC_SECRET)" | jq '{bookedMinor, availableMinor}'
```

## Step 6: see what one receipt wrote

```sh
gtbsql -d payment -v p="$PAYMENT_ID"
```

```sql
\pset pager off
SELECT lifecycle_state, execution_outcome, version, claim_kind, execution_workflow_id FROM app.payments WHERE payment_id = :'p';
SELECT product, debtor_account_id, amount_minor, currency, creditor_name, reference FROM app.payment_instructions WHERE payment_id = :'p';
SELECT version, change_kind, from_state, to_state, actor_type, policy_version, evidence FROM app.payment_versions WHERE payment_id = :'p' ORDER BY version;
SELECT channel, customer_id, idempotency_key, left(request_hash, 12) AS hash FROM app.idempotency_records WHERE payment_id = :'p';
SELECT command_type, status, committed_version, correlation_id FROM app.commands WHERE payment_id = :'p';
SELECT action, actor_type, aggregate_version, details FROM app.audit_entries WHERE aggregate_id = :'p';
SELECT e.event_type, e.aggregate_version, e.event_sequence, e.payload, o.published_at
  FROM app.domain_events e JOIN app.outbox o USING (event_id) WHERE e.aggregate_id = :'p';
SELECT workflow_id, status, attempts, next_attempt_at FROM app.workflow_dispatch WHERE payment_id = :'p';
```

| Table | Row for this payment | Why it exists |
| --- | --- | --- |
| `payments` | current state, `version 1` | The aggregate; updated only through versioned changes |
| `payment_instructions` | what the customer asked for | Immutable; what later phases execute |
| `payment_versions` | version 1, `RECEIVED`, actor `CUSTOMER`, policy `payment-receipt/v1`, evidence = command | Invariant 7: every change records who, why, under which policy and with what evidence |
| `idempotency_records` | scope + request hash | Replay/409 decisions; never expires |
| `commands` | `SUBMIT_PAYMENT`, `COMMITTED` | Backs `GET /commands/{id}` |
| `audit_entries` | `RECEIVE_PAYMENT` | Audit trail |
| `domain_events` + `outbox` | `PaymentReceived`, sequence 1, `published_at` NULL | B03's relay will publish it to Kafka |
| `workflow_dispatch` | `payment/{id}`, `PENDING` | B06's dispatcher will start the Temporal workflow |

Note that `correlation_id` in `commands` matches the `X-Correlation-Id` response
header. Correlation IDs let you trace one request across logs, events and tables.

The database refuses to rewrite history, even for the `gtb_admin` superuser:

```sql
BEGIN;
UPDATE app.payment_instructions SET amount_minor = 1 WHERE payment_id = :'p';   -- ERROR: payment_instructions is append-only
ROLLBACK;
BEGIN;
UPDATE app.payments SET version = version + 1 WHERE payment_id = :'p';          -- ERROR at COMMIT: no matching history row
COMMIT;
```

## Step 7: failure experiments

**Restart.** Stop payment-api (Ctrl-C in terminal B) and start it again. The
receipt read still shows `PENDING`, and the Step 2 request replays with the same
`paymentId`. The state lives in PostgreSQL, not in memory.

**Core Banking outage.** Stop terminal A, then:

```sh
curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $(newid)" \
  -H 'Content-Type: application/json' -d "$BODY" | jq .error
# 503 DEPENDENCY_UNAVAILABLE, retryable: true: ownership can't be verified, so nothing is written
curl -s -D - -o /dev/null $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' -d "$BODY" | grep -i replayed
# Idempotent-Replayed: true: replays are answered from the payment database alone
```

Restart terminal A afterwards.

**Concurrency, atomicity and the claim race.** These are hard to trigger by hand.
`backend/internal/payment/storage/postgres_integration_test.go` covers them:
16 concurrent duplicates produce one receipt; a failure injected after each of the
eight inserts leaves zero rows; and a submission racing a cancellation produces
exactly one winner. Run `make payment-integration` and read the tests.

## What is not here yet

- No workflow runs, no funds or limits are reserved, and nothing is screened.
  `RECEIVED` is where a B02 payment stays. B04–B06 add the rest.
- No payment list or search endpoint (B03 projections). No cancellation API
  (B06), though the claim primitive exists.
- Events sit in `outbox` unpublished until B03 adds the relay.
