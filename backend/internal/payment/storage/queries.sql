-- name: LockIdempotencyScope :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(scope), 0));

-- name: GetIdempotencyRecord :one
SELECT channel, customer_id, idempotency_key, request_hash, payment_id, command_id, recorded_at
FROM app.idempotency_records
WHERE channel = $1 AND customer_id = $2 AND idempotency_key = $3;

-- name: InsertPayment :exec
INSERT INTO app.payments (
    payment_id, customer_id, product, instruction_hash, lifecycle_state, execution_outcome,
    version, last_event_sequence, received_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertInstruction :exec
INSERT INTO app.payment_instructions (
    payment_id, instruction_hash, customer_id, product, debtor_account_id, amount_minor, currency,
    creditor_name, creditor_account_id, creditor_account_number, creditor_bank_identifier,
    creditor_country, reference
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: InsertPaymentVersion :exec
INSERT INTO app.payment_versions (
    payment_id, version, change_kind, from_state, to_state, from_outcome, to_outcome,
    actor_type, actor_subject, policy_version, reason, evidence, command_id, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: InsertCommand :exec
INSERT INTO app.commands (
    command_id, command_type, payment_id, customer_id, actor_type, actor_subject, status,
    committed_version, correlation_id, recorded_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertIdempotencyRecord :exec
INSERT INTO app.idempotency_records (
    channel, customer_id, idempotency_key, request_hash, payment_id, command_id, recorded_at
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertAuditEntry :exec
INSERT INTO app.audit_entries (
    audit_id, occurred_at, actor_type, actor_subject, action, aggregate_type, aggregate_id,
    aggregate_version, command_id, correlation_id, details
) VALUES ($1, $2, $3, $4, $5, 'Payment', $6, $7, $8, $9, $10);

-- name: InsertDomainEvent :exec
INSERT INTO app.domain_events (
    event_id, event_type, schema_version, aggregate_type, aggregate_id, aggregate_version,
    event_sequence, occurred_at, correlation_id, causation_id, source, payload
) VALUES ($1, $2, 1, 'Payment', $3, $4, $5, $6, $7, $8, 'payment-api', $9);

-- name: InsertOutbox :exec
INSERT INTO app.outbox (event_id, occurred_at) VALUES ($1, $2);

-- name: InsertWorkflowDispatch :exec
INSERT INTO app.workflow_dispatch (
    dispatch_id, payment_id, workflow_id, workflow_type, status, next_attempt_at, created_at, updated_at
) VALUES ($1, $2, $3, 'PaymentWorkflow', 'PENDING', $4, $4, $4);

-- name: GetPayment :one
SELECT * FROM app.payments WHERE payment_id = $1;

-- name: LockPayment :one
SELECT * FROM app.payments WHERE payment_id = $1 FOR UPDATE;

-- name: UpdatePayment :execrows
UPDATE app.payments
SET lifecycle_state = sqlc.arg(lifecycle_state), execution_outcome = sqlc.arg(execution_outcome),
    version = sqlc.arg(next_version), last_event_sequence = sqlc.arg(last_event_sequence),
    execution_workflow_id = sqlc.narg(execution_workflow_id), claim_kind = sqlc.narg(claim_kind),
    claim_id = sqlc.narg(claim_id), claimed_at = sqlc.narg(claimed_at), updated_at = sqlc.arg(updated_at)
WHERE payment_id = sqlc.arg(payment_id) AND version = sqlc.arg(expected_version);

-- name: GetInstruction :one
SELECT * FROM app.payment_instructions WHERE payment_id = $1;

-- name: GetCommand :one
SELECT * FROM app.commands WHERE command_id = $1;

-- name: GetWorkflowDispatch :one
SELECT * FROM app.workflow_dispatch WHERE payment_id = $1;

-- name: ListPaymentVersions :many
SELECT * FROM app.payment_versions WHERE payment_id = $1 ORDER BY version;
