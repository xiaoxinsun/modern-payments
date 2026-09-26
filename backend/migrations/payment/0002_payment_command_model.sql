-- B02 payment command model: aggregate, immutable instruction, version history,
-- scoped idempotency, command receipts, audit, domain events/outbox and
-- workflow-start dispatch intent. All rows for one receipt commit together.

CREATE TABLE app.payments (
    payment_id uuid PRIMARY KEY,
    customer_id uuid NOT NULL,
    product text NOT NULL CHECK (product IN ('BOOK_TRANSFER', 'DOMESTIC_INSTANT', 'RTGS', 'CROSS_BORDER')),
    instruction_hash text NOT NULL CHECK (instruction_hash ~ '^[0-9a-f]{64}$'),
    lifecycle_state text NOT NULL CHECK (lifecycle_state IN (
        'RECEIVED', 'VALIDATING', 'SCHEDULED', 'REQUIRES_REVIEW', 'ACCEPTED',
        'EXECUTING', 'COMPLETED', 'REJECTED', 'CANCELLED', 'RETURNED'
    )),
    execution_outcome text NOT NULL CHECK (execution_outcome IN (
        'NOT_SUBMITTED', 'PENDING', 'CONFIRMED_SUCCESS', 'CONFIRMED_FAILURE', 'UNKNOWN'
    )),
    version bigint NOT NULL CHECK (version BETWEEN 1 AND 999999999999999999),
    last_event_sequence bigint NOT NULL CHECK (last_event_sequence BETWEEN 1 AND 999999999999999999),
    execution_workflow_id text CHECK (execution_workflow_id ~ '^execution/[0-9a-f-]{36}/[a-z-]+$'),
    claim_kind text CHECK (claim_kind IN ('SUBMISSION', 'CANCELLATION')),
    claim_id uuid UNIQUE,
    claimed_at timestamptz,
    received_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK ((claim_kind IS NULL) = (claim_id IS NULL) AND (claim_id IS NULL) = (claimed_at IS NULL))
);

CREATE INDEX payments_customer_idx ON app.payments(customer_id, received_at DESC, payment_id);

CREATE TABLE app.payment_instructions (
    payment_id uuid PRIMARY KEY REFERENCES app.payments(payment_id),
    instruction_hash text NOT NULL CHECK (instruction_hash ~ '^[0-9a-f]{64}$'),
    customer_id uuid NOT NULL,
    product text NOT NULL,
    debtor_account_id uuid NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 999999999999999999),
    currency text NOT NULL CHECK (currency IN ('SGD', 'USD')),
    creditor_name text NOT NULL CHECK (length(creditor_name) BETWEEN 1 AND 140),
    creditor_account_id uuid,
    creditor_account_number text CHECK (length(creditor_account_number) BETWEEN 1 AND 34),
    creditor_bank_identifier text CHECK (length(creditor_bank_identifier) BETWEEN 4 AND 11),
    creditor_country text CHECK (creditor_country ~ '^[A-Z]{2}$'),
    reference text CHECK (length(reference) BETWEEN 1 AND 140),
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((product = 'BOOK_TRANSFER') = (creditor_account_id IS NOT NULL)),
    CHECK ((product = 'BOOK_TRANSFER') = (creditor_account_number IS NULL))
);

-- One row per aggregate version: evidence, actor, policy version (invariant 7).
CREATE TABLE app.payment_versions (
    payment_id uuid NOT NULL REFERENCES app.payments(payment_id),
    version bigint NOT NULL CHECK (version BETWEEN 1 AND 999999999999999999),
    change_kind text NOT NULL CHECK (change_kind IN (
        'RECEIVED', 'TRANSITION', 'SUBMISSION_CLAIMED', 'CANCELLATION_CLAIMED', 'EXECUTION_OWNER_ASSIGNED'
    )),
    from_state text,
    to_state text NOT NULL,
    from_outcome text NOT NULL,
    to_outcome text NOT NULL,
    actor_type text NOT NULL CHECK (actor_type IN ('CUSTOMER', 'OPERATOR', 'SYSTEM')),
    actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 256),
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 64),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 256),
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'array'),
    command_id uuid,
    occurred_at timestamptz NOT NULL,
    PRIMARY KEY (payment_id, version),
    CHECK ((version = 1) = (change_kind = 'RECEIVED'))
);

CREATE TABLE app.commands (
    command_id uuid PRIMARY KEY,
    command_type text NOT NULL CHECK (command_type IN ('SUBMIT_PAYMENT')),
    payment_id uuid NOT NULL REFERENCES app.payments(payment_id),
    customer_id uuid NOT NULL,
    actor_type text NOT NULL CHECK (actor_type IN ('CUSTOMER', 'OPERATOR', 'SYSTEM')),
    actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 256),
    status text NOT NULL CHECK (status IN ('COMMITTED')),
    committed_version bigint NOT NULL CHECK (committed_version >= 1),
    correlation_id uuid NOT NULL,
    recorded_at timestamptz NOT NULL
);

CREATE INDEX commands_payment_idx ON app.commands(payment_id);

-- Duplicate identities have no TTL: a tombstone must outlive every retry horizon.
CREATE TABLE app.idempotency_records (
    channel text NOT NULL CHECK (channel IN ('CUSTOMER_API')),
    customer_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    payment_id uuid NOT NULL UNIQUE REFERENCES app.payments(payment_id),
    command_id uuid NOT NULL UNIQUE REFERENCES app.commands(command_id),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (channel, customer_id, idempotency_key)
);

CREATE TABLE app.audit_entries (
    audit_id uuid PRIMARY KEY,
    occurred_at timestamptz NOT NULL,
    actor_type text NOT NULL CHECK (actor_type IN ('CUSTOMER', 'OPERATOR', 'SYSTEM')),
    actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 256),
    action text NOT NULL CHECK (action ~ '^[A-Z][A-Z_]{0,63}$'),
    aggregate_type text NOT NULL CHECK (aggregate_type = 'Payment'),
    aggregate_id uuid NOT NULL REFERENCES app.payments(payment_id),
    aggregate_version bigint NOT NULL CHECK (aggregate_version >= 1),
    command_id uuid,
    correlation_id uuid NOT NULL,
    details jsonb NOT NULL CHECK (jsonb_typeof(details) = 'object'),
    FOREIGN KEY (aggregate_id, aggregate_version) REFERENCES app.payment_versions(payment_id, version)
);

CREATE INDEX audit_entries_aggregate_idx ON app.audit_entries(aggregate_id, aggregate_version);

CREATE TABLE app.domain_events (
    event_id uuid PRIMARY KEY,
    event_type text NOT NULL CHECK (event_type ~ '^[A-Za-z][A-Za-z0-9._-]{0,127}$'),
    schema_version integer NOT NULL CHECK (schema_version = 1),
    aggregate_type text NOT NULL CHECK (aggregate_type = 'Payment'),
    aggregate_id uuid NOT NULL REFERENCES app.payments(payment_id),
    aggregate_version bigint NOT NULL CHECK (aggregate_version >= 1),
    event_sequence bigint NOT NULL CHECK (event_sequence BETWEEN 1 AND 999999999999999999),
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    correlation_id uuid NOT NULL,
    causation_id uuid NOT NULL,
    source text NOT NULL CHECK (source = 'payment-api'),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    UNIQUE (aggregate_id, event_sequence),
    FOREIGN KEY (aggregate_id, aggregate_version) REFERENCES app.payment_versions(payment_id, version)
);

CREATE TABLE app.outbox (
    event_id uuid PRIMARY KEY REFERENCES app.domain_events(event_id),
    occurred_at timestamptz NOT NULL,
    published_at timestamptz,
    delivery_attempts integer NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
    last_error text,
    CHECK (published_at IS NULL OR last_error IS NULL)
);

CREATE INDEX outbox_pending_idx ON app.outbox(occurred_at, event_id) WHERE published_at IS NULL;

-- Durable intent to start payment/{paymentId}. Delivery state is separate from
-- the event outbox; PENDING means received, never accepted.
CREATE TABLE app.workflow_dispatch (
    dispatch_id uuid PRIMARY KEY,
    payment_id uuid NOT NULL UNIQUE REFERENCES app.payments(payment_id),
    workflow_id text NOT NULL UNIQUE CHECK (workflow_id ~ '^payment/[0-9a-f-]{36}$'),
    workflow_type text NOT NULL CHECK (workflow_type = 'PaymentWorkflow'),
    status text NOT NULL CHECK (status IN ('PENDING', 'STARTED')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner text,
    lease_expires_at timestamptz,
    next_attempt_at timestamptz NOT NULL,
    last_error text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX workflow_dispatch_pending_idx ON app.workflow_dispatch(next_attempt_at, dispatch_id)
WHERE status = 'PENDING';

CREATE FUNCTION app.reject_immutable_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER payment_instructions_immutable BEFORE UPDATE OR DELETE ON app.payment_instructions
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();
CREATE TRIGGER payment_versions_immutable BEFORE UPDATE OR DELETE ON app.payment_versions
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();
CREATE TRIGGER idempotency_records_immutable BEFORE UPDATE OR DELETE ON app.idempotency_records
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();
CREATE TRIGGER audit_entries_immutable BEFORE UPDATE OR DELETE ON app.audit_entries
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();
CREATE TRIGGER domain_events_immutable BEFORE UPDATE OR DELETE ON app.domain_events
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();

-- Payments are retained; identity is fixed; every update advances exactly one
-- version and must be accompanied by that version's history row.
CREATE FUNCTION app.guard_payment_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
      RAISE EXCEPTION 'payments are retained records' USING ERRCODE = '55000';
    END IF;
    IF OLD.payment_id <> NEW.payment_id OR OLD.customer_id <> NEW.customer_id
       OR OLD.product <> NEW.product OR OLD.instruction_hash <> NEW.instruction_hash
       OR OLD.received_at <> NEW.received_at
       OR NEW.version <> OLD.version + 1
       OR NEW.last_event_sequence < OLD.last_event_sequence
       OR (OLD.claim_id IS NOT NULL AND (NEW.claim_id IS DISTINCT FROM OLD.claim_id
           OR NEW.claim_kind IS DISTINCT FROM OLD.claim_kind OR NEW.claimed_at IS DISTINCT FROM OLD.claimed_at))
       OR (OLD.execution_workflow_id IS NOT NULL AND NEW.execution_workflow_id IS DISTINCT FROM OLD.execution_workflow_id)
    THEN
      RAISE EXCEPTION 'payment identity, claim, owner or version sequence is immutable'
        USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_update_guard BEFORE UPDATE OR DELETE ON app.payments
FOR EACH ROW EXECUTE FUNCTION app.guard_payment_update();

CREATE FUNCTION app.require_payment_version_history() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM app.payment_versions
        WHERE payment_id = NEW.payment_id AND version = NEW.version
          AND to_state = NEW.lifecycle_state AND to_outcome = NEW.execution_outcome
    ) THEN
      RAISE EXCEPTION 'payment % version % has no matching history row', NEW.payment_id, NEW.version
        USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER payments_version_history
AFTER INSERT OR UPDATE ON app.payments
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION app.require_payment_version_history();
