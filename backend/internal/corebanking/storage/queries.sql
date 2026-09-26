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

