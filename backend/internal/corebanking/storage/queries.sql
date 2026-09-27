-- name: LockOperationIdentity :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(operation_id), 0));

-- name: GetOperation :one
SELECT operation_id, operation_type, request_hash, resource_type, resource_id,
       correlation_id, committed_at
FROM app.operations
WHERE operation_id = $1;

-- name: InsertOperation :one
INSERT INTO app.operations (
    operation_id, operation_type, request_hash, resource_type, resource_id, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING operation_id, operation_type, request_hash, resource_type, resource_id,
          correlation_id, committed_at;

-- name: GetAccount :one
SELECT account_id, external_reference, customer_id, account_kind, currency,
       booked_balance_minor, status, created_at, updated_at
FROM app.accounts
WHERE account_id = $1;

-- name: LockAccount :one
SELECT account_id, external_reference, customer_id, account_kind, currency,
       booked_balance_minor, status, created_at, updated_at
FROM app.accounts
WHERE account_id = $1
FOR UPDATE;

-- name: SumActiveHolds :one
SELECT coalesce(sum(amount_minor), 0)::bigint
FROM app.holds
WHERE account_id = $1 AND status = 'ACTIVE';

-- name: InsertHold :one
INSERT INTO app.holds (
    hold_id, account_id, reserve_operation_id, amount_minor, currency, status, expires_at
) VALUES ($1, $2, $3, $4, $5, 'ACTIVE', $6)
RETURNING hold_id, account_id, reserve_operation_id, amount_minor, currency, status,
          expires_at, consumed_by_operation_id, released_by_operation_id, created_at, updated_at;

-- name: GetHold :one
SELECT hold_id, account_id, reserve_operation_id, amount_minor, currency, status,
       expires_at, consumed_by_operation_id, released_by_operation_id, created_at, updated_at
FROM app.holds
WHERE hold_id = $1;

-- name: LockHold :one
SELECT hold_id, account_id, reserve_operation_id, amount_minor, currency, status,
       expires_at, consumed_by_operation_id, released_by_operation_id, created_at, updated_at
FROM app.holds
WHERE hold_id = $1
FOR UPDATE;

-- name: ConsumeHold :one
UPDATE app.holds
SET status = 'CONSUMED', consumed_by_operation_id = $2, updated_at = now()
WHERE hold_id = $1 AND status = 'ACTIVE'
RETURNING hold_id, account_id, reserve_operation_id, amount_minor, currency, status,
          expires_at, consumed_by_operation_id, released_by_operation_id, created_at, updated_at;

-- name: ReleaseHold :one
UPDATE app.holds
SET status = 'RELEASED', released_by_operation_id = $2, updated_at = now()
WHERE hold_id = $1 AND status = 'ACTIVE'
RETURNING hold_id, account_id, reserve_operation_id, amount_minor, currency, status,
          expires_at, consumed_by_operation_id, released_by_operation_id, created_at, updated_at;

-- name: InsertPosting :one
INSERT INTO app.posting_journals (
    posting_id, operation_id, description, reversal_of_posting_id
) VALUES ($1, $2, $3, $4)
RETURNING posting_id, operation_id, description, reversal_of_posting_id, committed_at;

-- name: GetPosting :one
SELECT posting_id, operation_id, description, reversal_of_posting_id, committed_at
FROM app.posting_journals
WHERE posting_id = $1;

-- name: InsertPostingEntry :exec
INSERT INTO app.posting_entries (
    posting_id, line_number, account_id, side, amount_minor, currency
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListPostingEntries :many
SELECT posting_id, line_number, account_id, side, amount_minor, currency
FROM app.posting_entries
WHERE posting_id = $1
ORDER BY line_number;

-- name: UpdateAccountBalance :one
UPDATE app.accounts
SET booked_balance_minor = $2, updated_at = now()
WHERE account_id = $1
RETURNING account_id, external_reference, customer_id, account_kind, currency,
          booked_balance_minor, status, created_at, updated_at;

-- name: InsertPostingEvent :one
INSERT INTO app.posting_events (
    event_id, event_type, schema_version, aggregate_type, aggregate_id,
    aggregate_version, event_sequence, occurred_at, correlation_id,
    causation_id, source, payload
) VALUES ($1, 'CoreBankingPostingCommitted', 1, 'CoreBankingPosting', $2,
          1, 1, $3, $4, $5, 'core-banking-service', $6)
RETURNING event_id, event_type, schema_version, aggregate_type, aggregate_id,
          aggregate_version, event_sequence, occurred_at, recorded_at,
          correlation_id, causation_id, source, payload;

-- name: InsertPostingOutbox :exec
INSERT INTO app.posting_outbox (event_id, occurred_at) VALUES ($1, $2);

-- name: GetPostingEvent :one
SELECT event_id, event_type, schema_version, aggregate_type, aggregate_id,
       aggregate_version, event_sequence, occurred_at, recorded_at,
       correlation_id, causation_id, source, payload
FROM app.posting_events
WHERE aggregate_id = $1;

-- name: ListStatement :many
SELECT e.posting_id, e.line_number, j.operation_id, j.description,
       j.reversal_of_posting_id, j.committed_at, e.side, e.amount_minor, e.currency
FROM app.posting_entries e
JOIN app.posting_journals j USING (posting_id)
WHERE e.account_id = $1
  AND ($2::timestamptz IS NULL OR j.committed_at >= $2)
  AND ($3::timestamptz IS NULL OR j.committed_at < $3)
ORDER BY j.committed_at, e.posting_id, e.line_number
LIMIT $4;


-- name: LockPendingPostingOutbox :many
SELECT e.event_id, e.event_type, e.schema_version, e.aggregate_type, e.aggregate_id,
       e.aggregate_version, e.event_sequence, e.occurred_at, e.recorded_at,
       e.correlation_id, e.causation_id, e.source, e.payload
FROM app.posting_outbox o
JOIN app.posting_events e ON e.event_id = o.event_id
WHERE o.published_at IS NULL
ORDER BY o.occurred_at, o.event_id
LIMIT sqlc.arg(batch_size)
FOR UPDATE OF o SKIP LOCKED;

-- name: MarkPostingOutboxPublished :exec
UPDATE app.posting_outbox
SET published_at = sqlc.arg(published_at), delivery_attempts = delivery_attempts + 1, last_error = NULL
WHERE event_id = ANY(sqlc.arg(event_ids)::uuid[]) AND published_at IS NULL;

-- name: MarkPostingOutboxFailed :exec
UPDATE app.posting_outbox
SET delivery_attempts = delivery_attempts + 1, last_error = sqlc.arg(last_error)
WHERE event_id = sqlc.arg(event_id) AND published_at IS NULL;
