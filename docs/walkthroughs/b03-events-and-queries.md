# Walkthrough: event delivery and payment queries (B03)

B02 wrote a `PaymentReceived` event into the payment database and stopped
there. B03 carries events onward:

- **relays** publish each owner's outbox to Kafka;
- the **event projector** turns payment events into query models in the
  separate `projections` database;
- the **query API** serves searches and timelines from those models.

The query models can lag behind the command store, so every answer says how
fresh it is.

Prerequisites: [shared setup](README.md) sections 1–3, including
`make kafka-topics`, with all five terminals running (A–E).

## Concepts in five minutes

- **Outbox relay.** A payment and its event commit in one transaction; the
  relay publishes afterwards. It marks a row `published_at` only after Kafka
  acknowledges it, so a crash or broker outage causes a republish, never a lost
  event. Delivery is *at least once*, with a stable `eventId`.
- **Topics and keys.** Payment events go to `gtb.payment.events.v1`, keyed by
  payment ID, so one payment's events stay in order on one partition. Core
  Banking postings go to `gtb.corebanking.postings.v1`.
- **Projector transaction.** One transaction per batch covers the inbox
  (dedupe by event ID), the summary and timeline changes, and the Kafka
  checkpoint. Replays and redeliveries therefore change nothing.
- **Ordering gaps and poison.**
  - An event that arrives ahead of a missing sequence waits in a buffer. If the
    gap persists, the projector fetches the missing events from the payment
    journal through payment-api.
  - A record that can never be projected goes to a repair queue instead of
    blocking its partition.
- **Generations and rebuild.**
  - Query tables are keyed by *generation*. `event-projector rebuild` fills a
    new generation from the journal while live events keep arriving, verifies
    it, then switches reads in one transaction.
  - Replayed events never reach effectful consumers, so a rebuild cannot move
    money or send notifications.
- **Freshness and pending.**
  - Answers carry `projection.freshness`: `CURRENT`, `LAGGING`, or `STALE`
    (the projector has not reached Kafka recently).
  - A payment that was received but is not projected yet is answered from the
    command store with `status: PENDING`, not 404.
- Design: [ADR 0010](../../spec/adr/0010-event-delivery-and-cqrs-queries.md).
  Contract: `contracts/openapi/v1/query.json`.

## Code map

| What | Where |
| --- | --- |
| Kafka producer, relay loop, replay guard | `backend/internal/eventing/` |
| Payment outbox `Deliver`, journal reads, upcaster | `backend/internal/payment/storage/journal.go` |
| Core Banking outbox `Deliver` | `backend/internal/corebanking/outbox.go` |
| Journal API handlers (`internal_service` only) | `backend/internal/payment/api/handler.go` |
| Projector: dedupe, gaps, poison, checkpoint | `backend/internal/projections/projector.go`, `consumer.go` |
| Gap catch-up and rebuild | `backend/internal/projections/catchup.go`, `rebuild.go` |
| Query service: scope, cursors, freshness, pending | `backend/internal/projections/query/service.go` |
| Query HTTP boundary (generated + handlers) | `backend/internal/projections/api/` |
| Process wiring | `backend/cmd/{payment-worker,event-projector,query-api}/main.go` |
| Tables | `backend/migrations/payment/0003_*.sql`, `backend/migrations/projections/0002_*.sql` |

## Step 1: follow one payment from receipt to query

```sh
PAY=http://localhost:8082/api/v1
QRY=http://localhost:8083/api/v1
MAYA_CUSTOMER=20000000-0000-4000-8000-000000000001
CUSTOMER=$(user_token customer.demo GTB_CUSTOMER_PASSWORD)
OPS=$(user_token operations.demo GTB_OPERATIONS_PASSWORD)

PAYMENT_ID=$(curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $(newid)" \
  -H 'Content-Type: application/json' -d '{
  "customerId":"'$MAYA_CUSTOMER'","product":"BOOK_TRANSFER",
  "debtorAccountId":"10000000-0000-4000-8000-000000000001",
  "amount":{"amountMinor":"1500","currency":"SGD"},
  "creditor":{"name":"Daniel","accountId":"10000000-0000-4000-8000-000000000002"},"reference":"B03 walkthrough"}' \
  | jq -r .paymentId)

sleep 2
curl -s $QRY/payments/$PAYMENT_ID -H "Authorization: Bearer $CUSTOMER" | jq .
```

Expect `"status":"PROJECTED"`, `"version":"1"`, `"lifecycleState":"RECEIVED"`, and
a `projection` block with `"freshness":"CURRENT"`, the active `generation` and
`"consumerLag":"0"`. Now see each hop.

The outbox row was acknowledged:

```sh
gtbsql -d payment -c "SELECT e.event_type, e.journal_position, o.published_at IS NOT NULL AS published, o.delivery_attempts
  FROM app.domain_events e JOIN app.outbox o USING (event_id) WHERE e.aggregate_id = '$PAYMENT_ID';"
```

The record is on Kafka, keyed by the payment ID:

```sh
docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml exec -T kafka \
  /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server localhost:19092 --topic gtb.payment.events.v1 \
  --from-beginning --timeout-ms 8000 --property print.key=true --property key.separator=' ' 2>/dev/null \
  | grep "^$PAYMENT_ID " | cut -d' ' -f2- | jq '{eventId, eventType, aggregateVersion, eventSequence, payload}'
```

The projection rows are in the `projections` database:

```sh
gtbsql -d projections -c "SELECT generation, lifecycle_state, projected_version, last_event_sequence FROM app.payment_summary WHERE payment_id = '$PAYMENT_ID';" \
  -c "SELECT generation, delivery, processed_at FROM app.inbox WHERE aggregate_id = '$PAYMENT_ID';"
```

## Step 2: the query API

```sh
curl -s $QRY/payments/$PAYMENT_ID/timeline -H "Authorization: Bearer $CUSTOMER" | jq '.entries'
curl -s "$QRY/payments?customerId=$MAYA_CUSTOMER&limit=2" -H "Authorization: Bearer $CUSTOMER" \
  | jq '{ids: [.items[].paymentId], nextCursor}'

# minVersion: a client that just committed version N waits until it is visible.
curl -s "$QRY/payments/$PAYMENT_ID?minVersion=2" -H "Authorization: Bearer $CUSTOMER" | jq .status   # "BEHIND"
```

Pass `nextCursor` back as `cursor=` for the next page. A cursor is bound to the
caller and the filters, so reusing it with other filters returns `400`. Scope
works as it does for receipts:

```sh
curl -s -o /dev/null -w "%{http_code}\n" "$QRY/payments?customerId=20000000-0000-4000-8000-000000000002" -H "Authorization: Bearer $CUSTOMER"  # 403: not your customer
curl -s -o /dev/null -w "%{http_code}\n" "$QRY/payments" -H "Authorization: Bearer $CUSTOMER"                                          # 400: customers must name a customerId
curl -s "$QRY/payments?limit=3" -H "Authorization: Bearer $OPS" | jq '[.items[] | {paymentId, customerId, lifecycleState}]'            # operators see all
curl -s $QRY/ops/projections -H "Authorization: Bearer $OPS" | jq '{generations, consumers: [.consumers[] | {consumer, freshness, partitions: [.partitions[] | {partition, nextOffset, lag}]}], bufferedEvents, openRepairs}'
```

## Step 3: stop the projector (terminal D)

Press Ctrl-C in terminal D, then:

```sh
PENDING_ID=$(curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $(newid)" \
  -H 'Content-Type: application/json' -d '{
  "customerId":"'$MAYA_CUSTOMER'","product":"BOOK_TRANSFER",
  "debtorAccountId":"10000000-0000-4000-8000-000000000001",
  "amount":{"amountMinor":"1600","currency":"SGD"},
  "creditor":{"name":"Daniel","accountId":"10000000-0000-4000-8000-000000000002"}}' | jq -r .paymentId)

curl -s $QRY/payments/$PENDING_ID -H "Authorization: Bearer $CUSTOMER" | jq '{status, lifecycleState, version}'
sleep 11
curl -s $QRY/payments/$PAYMENT_ID -H "Authorization: Bearer $CUSTOMER" | jq .projection.freshness   # "STALE"
```

The new payment is `PENDING`: the query API asked payment-api for the receipt,
forwarding your own token, so the owner still authorizes the read. Older
payments are still served, marked `STALE`. Restart terminal D. Within a few
seconds the pending payment is projected and freshness returns to `CURRENT`.
The projector resumed from the checkpoint it stored with its last batch.

```sh
sleep 5
curl -s $QRY/payments/$PENDING_ID -H "Authorization: Bearer $CUSTOMER" | jq '{status, projection: .projection.freshness}'
```

## Step 4: stop Kafka

```sh
docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml stop kafka

OUTAGE_ID=$(curl -s $PAY/payments -H "Authorization: Bearer $CUSTOMER" -H "Idempotency-Key: $(newid)" \
  -H 'Content-Type: application/json' -d '{
  "customerId":"'$MAYA_CUSTOMER'","product":"BOOK_TRANSFER",
  "debtorAccountId":"10000000-0000-4000-8000-000000000001",
  "amount":{"amountMinor":"1700","currency":"SGD"},
  "creditor":{"name":"Daniel","accountId":"10000000-0000-4000-8000-000000000002"}}' | jq -r .paymentId)
echo $OUTAGE_ID     # the receipt still commits

sleep 25
gtbsql -d payment -c "SELECT o.published_at, o.delivery_attempts, left(o.last_error, 60) AS last_error
  FROM app.outbox o JOIN app.domain_events e USING (event_id) WHERE e.aggregate_id = '$OUTAGE_ID';"
curl -s $QRY/payments/$OUTAGE_ID -H "Authorization: Bearer $CUSTOMER" | jq '{status, freshness: .projection.freshness}'   # PENDING, STALE
```

The event waits in the outbox with its failed attempts recorded (terminal C
logs the retries). Bring Kafka back; the relay publishes, the projector
catches up, and freshness recovers:

```sh
docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml up -d --wait kafka
sleep 15
curl -s $QRY/payments/$OUTAGE_ID -H "Authorization: Bearer $CUSTOMER" | jq '{status, freshness: .projection.freshness}'   # PROJECTED, CURRENT
```

## Step 5: redelivery changes nothing

Make the relay publish Step 1's event again, as it would after a crash between
Kafka's acknowledgement and its own commit:

```sh
gtbsql -d projections -c "SELECT count(*) AS timeline_rows FROM app.payment_timeline WHERE payment_id = '$PAYMENT_ID';"
gtbsql -d payment -c "UPDATE app.outbox SET published_at = NULL WHERE event_id =
  (SELECT event_id FROM app.domain_events WHERE aggregate_id = '$PAYMENT_ID' AND event_sequence = 1);"
sleep 3
gtbsql -d payment -c "SELECT o.delivery_attempts, o.published_at IS NOT NULL AS published FROM app.outbox o
  JOIN app.domain_events e USING (event_id) WHERE e.aggregate_id = '$PAYMENT_ID';"
gtbsql -d projections -c "SELECT count(*) AS timeline_rows FROM app.payment_timeline WHERE payment_id = '$PAYMENT_ID';"
```

The event went out a second time (`delivery_attempts` 2) with the same `eventId`.
The projector's inbox recognized it, so the timeline still has one row per
generation. A crash after the projector's commit but before Kafka's
acknowledgement is absorbed the same way.

## Step 6: a poison record

```sh
echo "poison-$(newid)|this is not an event" | docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml exec -T kafka \
  /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server localhost:19092 --topic gtb.payment.events.v1 \
  --property parse.key=true --property key.separator='|'
sleep 2
gtbsql -d projections -c "SELECT reason, topic, partition, record_offset, status FROM app.repair_queue ORDER BY first_seen_at DESC LIMIT 1;"
curl -s $QRY/ops/projections -H "Authorization: Bearer $OPS" | jq .openRepairs
```

The record was quarantined and its partition kept moving. Once you have looked
at it, mark it handled:

```sh
gtbsql -d projections -c "UPDATE app.repair_queue SET status = 'RESOLVED' WHERE record_value = 'this is not an event';"
```

## Step 7: rebuild the projections while payments arrive

Keep terminal D running: live events must reach the new generation while it is
built. In another terminal with the helpers loaded:

```sh
GTB_DATABASE_URL="postgresql://projections:$(gtbenv PROJECTIONS_DB_PASSWORD)@127.0.0.1:5432/projections?sslmode=disable" \
GTB_SERVICE_CLIENT_SECRET="$(gtbenv EVENT_PROJECTOR_OIDC_SECRET)" \
.build/bin/event-projector rebuild 2>/dev/null | jq '{generation, watermark, projectedPayments, projectedEvents, sampledTimelines, problems, activated, retiredGeneration}'
```

The report shows:

- the journal watermark it replayed up to. `position` can exceed `events`,
  because rolled-back transactions consume journal positions;
- the counts it verified against the journal;
- how many timelines it compared;
- `"activated": true`.

Reads have switched to the new generation. The history of generations is in
the database:

```sh
gtbsql -d projections -c "SELECT generation, status, journal_watermark, activated_at, finished_at FROM app.projection_generations ORDER BY generation;"
curl -s $QRY/payments/$PAYMENT_ID -H "Authorization: Bearer $CUSTOMER" | jq '{status, version, generation: .projection.generation}'
```

Nothing was posted to the ledger, and no payment events were written, since
replayed events never reach effectful consumers. If a rebuild fails
verification, the active generation is untouched and the report lists the
`problems`.

## Step 8: Core Banking postings on Kafka

Core Banking relays its own outbox. Post and reverse a small transfer (the
balances end where they started), then find both events on their topic:

```sh
CB=http://localhost:8084/integrations/v1/core-banking
SERVICE=$(service_token payment-service PAYMENT_SERVICE_OIDC_SECRET)
POSTING_ID=$(curl -s $CB/postings -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' -d '{
  "operationId":"'$(newid)'","description":"B03 walkthrough","entries":[
  {"accountId":"10000000-0000-4000-8000-000000000001","side":"DEBIT","amount":{"amountMinor":"1","currency":"SGD"}},
  {"accountId":"10000000-0000-4000-8000-000000000002","side":"CREDIT","amount":{"amountMinor":"1","currency":"SGD"}}]}' | jq -r .postingId)
curl -s $CB/reversals -H "Authorization: Bearer $SERVICE" -H 'Content-Type: application/json' \
  -d '{"operationId":"'$(newid)'","originalPostingId":"'$POSTING_ID'","description":"B03 walkthrough reversal"}' > /dev/null
sleep 2
docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml exec -T kafka \
  /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server localhost:19092 --topic gtb.corebanking.postings.v1 \
  --from-beginning --timeout-ms 8000 2>/dev/null | jq -c 'select(.aggregateId == "'$POSTING_ID'" or .payload.reversalOfPostingId == "'$POSTING_ID'") | {eventType, aggregateId, reversalOf: .payload.reversalOfPostingId}'
```

Nothing consumes these yet. B06 will link them to payments.

## Where this goes next

- **B04–B06** add payment events beyond `PaymentReceived`. Each carries its
  resulting state, and the projector applies it with no code change.
- **B15** adds effectful consumers (notifications), which the replay guard
  keeps away from rebuilds.
- **F02** polls this API with `minVersion` after each command.
