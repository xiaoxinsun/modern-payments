-- Projector (projections owner login) ---------------------------------------

-- name: OpenGenerations :many
SELECT generation, status FROM app.projection_generations
WHERE status IN ('ACTIVE', 'BUILDING')
ORDER BY generation;

-- name: LockAggregate :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: InboxContains :one
SELECT EXISTS (SELECT 1 FROM app.inbox WHERE generation = $1 AND event_id = $2);

-- name: SummaryProgress :one
SELECT last_event_sequence FROM app.payment_summary WHERE generation = $1 AND payment_id = $2;

-- name: InsertSummary :exec
INSERT INTO app.payment_summary (
    generation, payment_id, customer_id, product, amount_minor, currency, lifecycle_state,
    execution_outcome, projected_version, last_event_sequence, received_at, last_event_at, projected_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: AdvanceSummary :execrows
UPDATE app.payment_summary
SET lifecycle_state = sqlc.arg(lifecycle_state), execution_outcome = sqlc.arg(execution_outcome),
    projected_version = sqlc.arg(projected_version), last_event_sequence = sqlc.arg(event_sequence),
    last_event_at = sqlc.arg(last_event_at), projected_at = sqlc.arg(projected_at)
WHERE generation = sqlc.arg(generation) AND payment_id = sqlc.arg(payment_id)
  AND last_event_sequence = sqlc.arg(event_sequence) - 1;

-- name: InsertTimeline :exec
INSERT INTO app.payment_timeline (
    generation, payment_id, event_sequence, event_id, event_type, aggregate_version, lifecycle_state,
    execution_outcome, occurred_at, recorded_at, correlation_id, causation_id, projected_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: InsertInbox :exec
INSERT INTO app.inbox (generation, event_id, aggregate_id, event_sequence, delivery, processed_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: BufferEvent :execrows
INSERT INTO app.pending_events (generation, event_id, aggregate_id, event_sequence, delivery, envelope, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (generation, event_id) DO NOTHING;

-- name: TakePendingThrough :many
DELETE FROM app.pending_events
WHERE generation = $1 AND aggregate_id = $2 AND event_sequence <= sqlc.arg(through_sequence)
RETURNING event_id, event_sequence, delivery, envelope;

-- name: InsertRepair :exec
INSERT INTO app.repair_queue (
    repair_id, consumer, topic, partition, record_offset, event_id, aggregate_id, event_sequence,
    reason, record_value, status, first_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'OPEN', $11)
ON CONFLICT DO NOTHING;

-- name: UpsertOffset :exec
INSERT INTO app.consumer_offsets (consumer, topic, partition, next_offset, high_watermark, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (consumer, topic, partition) DO UPDATE
SET next_offset = GREATEST(app.consumer_offsets.next_offset, EXCLUDED.next_offset),
    high_watermark = GREATEST(app.consumer_offsets.high_watermark, EXCLUDED.high_watermark),
    updated_at = EXCLUDED.updated_at;

-- name: ListOffsets :many
SELECT partition, next_offset FROM app.consumer_offsets WHERE consumer = $1 AND topic = $2;

-- name: TouchConsumer :exec
INSERT INTO app.consumer_status (consumer, broker_seen_at, last_event_at)
VALUES ($1, $2, sqlc.narg(last_event_at))
ON CONFLICT (consumer) DO UPDATE
SET broker_seen_at = EXCLUDED.broker_seen_at,
    last_event_at = COALESCE(EXCLUDED.last_event_at, app.consumer_status.last_event_at);

-- name: GapCandidates :many
SELECT p.generation, p.aggregate_id, COALESCE(s.last_event_sequence, 0)::bigint AS projected_sequence,
       min(p.received_at)::timestamptz AS oldest, count(*)::bigint AS buffered
FROM app.pending_events p
LEFT JOIN app.payment_summary s ON s.generation = p.generation AND s.payment_id = p.aggregate_id
GROUP BY p.generation, p.aggregate_id, s.last_event_sequence
HAVING min(p.received_at) <= sqlc.arg(older_than) OR count(*) >= sqlc.arg(buffer_limit)::bigint
ORDER BY min(p.received_at)
LIMIT sqlc.arg(max_aggregates);

-- Rebuild ---------------------------------------------------------------------

-- name: StartGeneration :one
INSERT INTO app.projection_generations (generation, status, created_at)
SELECT COALESCE(max(generation), 0) + 1, 'BUILDING', sqlc.arg(created_at)::timestamptz FROM app.projection_generations
RETURNING generation;

-- name: SetWatermark :exec
UPDATE app.projection_generations SET journal_watermark = $2 WHERE generation = $1 AND status = 'BUILDING';

-- name: GenerationCounts :one
SELECT (SELECT count(*) FROM app.payment_summary s WHERE s.generation = sqlc.arg(generation))::bigint AS payments,
       (SELECT count(*) FROM app.inbox i WHERE i.generation = sqlc.arg(generation))::bigint AS events,
       (SELECT count(*) FROM app.pending_events p WHERE p.generation = sqlc.arg(generation))::bigint AS buffered;

-- name: SampleAggregates :many
(SELECT s.payment_id FROM app.payment_summary s WHERE s.generation = sqlc.arg(generation)
 ORDER BY s.received_at DESC, s.payment_id DESC LIMIT sqlc.arg(sample_size))
UNION
(SELECT r.payment_id FROM app.payment_summary r WHERE r.generation = sqlc.arg(generation)
 ORDER BY random() LIMIT sqlc.arg(sample_size));

-- name: GenerationTimeline :many
SELECT event_id, event_sequence, event_type, aggregate_version
FROM app.payment_timeline WHERE generation = $1 AND payment_id = $2
ORDER BY event_sequence;

-- name: RetireActive :one
UPDATE app.projection_generations SET status = 'RETIRED', finished_at = sqlc.arg(finished_at)
WHERE status = 'ACTIVE'
RETURNING generation;

-- name: ActivateGeneration :execrows
UPDATE app.projection_generations
SET status = 'ACTIVE', activated_at = sqlc.arg(activated_at), verification = sqlc.arg(verification)
WHERE generation = sqlc.arg(generation) AND status = 'BUILDING';

-- name: FailGeneration :exec
UPDATE app.projection_generations
SET status = 'FAILED', finished_at = sqlc.arg(finished_at), verification = sqlc.arg(verification)
WHERE generation = sqlc.arg(generation) AND status = 'BUILDING';

-- name: DeleteGenerationTimeline :exec
DELETE FROM app.payment_timeline WHERE generation = $1;

-- name: DeleteGenerationSummary :exec
DELETE FROM app.payment_summary WHERE generation = $1;

-- name: DeleteGenerationInbox :exec
DELETE FROM app.inbox WHERE generation = $1;

-- name: DeleteGenerationPending :exec
DELETE FROM app.pending_events WHERE generation = $1;

-- Query API (query_reader login) ----------------------------------------------

-- name: ActivePayment :one
SELECT s.* FROM app.payment_summary s
JOIN app.projection_generations g ON g.generation = s.generation AND g.status = 'ACTIVE'
WHERE s.payment_id = $1;

-- name: ListActivePayments :many
SELECT s.* FROM app.payment_summary s
JOIN app.projection_generations g ON g.generation = s.generation AND g.status = 'ACTIVE'
WHERE (sqlc.narg(customer_ids)::uuid[] IS NULL OR s.customer_id = ANY(sqlc.narg(customer_ids)::uuid[]))
  AND (sqlc.narg(lifecycle_state)::text IS NULL OR s.lifecycle_state = sqlc.narg(lifecycle_state)::text)
  AND (sqlc.narg(before_received_at)::timestamptz IS NULL
       OR (s.received_at, s.payment_id) < (sqlc.narg(before_received_at)::timestamptz, sqlc.narg(before_payment_id)::uuid))
ORDER BY s.received_at DESC, s.payment_id DESC
LIMIT sqlc.arg(page_size);

-- name: ActiveTimeline :many
SELECT t.* FROM app.payment_timeline t
JOIN app.projection_generations g ON g.generation = t.generation AND g.status = 'ACTIVE'
WHERE t.payment_id = $1
ORDER BY t.event_sequence;

-- name: ListGenerations :many
SELECT generation, status, journal_watermark, created_at, activated_at, finished_at
FROM app.projection_generations
WHERE status IN ('ACTIVE', 'BUILDING') OR finished_at > now() - interval '1 day'
ORDER BY generation DESC
LIMIT 10;

-- name: ConsumerLag :many
SELECT o.consumer, o.topic, o.partition, o.next_offset, o.high_watermark, o.updated_at
FROM app.consumer_offsets o
ORDER BY o.consumer, o.topic, o.partition;

-- name: ConsumerStatuses :many
SELECT consumer, broker_seen_at, last_event_at FROM app.consumer_status ORDER BY consumer;

-- name: DeliveryBacklog :one
SELECT (SELECT count(*) FROM app.pending_events)::bigint AS buffered,
       (SELECT min(received_at) FROM app.pending_events)::timestamptz AS oldest_buffered_at,
       (SELECT count(*) FROM app.repair_queue WHERE status = 'OPEN')::bigint AS open_repairs,
       (SELECT min(first_seen_at) FROM app.repair_queue WHERE status = 'OPEN')::timestamptz AS oldest_repair_at;
