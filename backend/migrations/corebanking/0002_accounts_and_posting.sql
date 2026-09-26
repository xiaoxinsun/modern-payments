CREATE TABLE app.accounts (
    account_id uuid PRIMARY KEY,
    external_reference text NOT NULL UNIQUE CHECK (length(external_reference) BETWEEN 1 AND 128),
    customer_id uuid,
    account_kind text NOT NULL CHECK (account_kind IN (
        'CUSTOMER_DEPOSIT', 'OUTGOING_CLEARING', 'SETTLEMENT_NOSTRO', 'INCOMING_SUSPENSE'
    )),
    currency text NOT NULL CHECK (currency IN ('SGD', 'USD')),
    booked_balance_minor bigint NOT NULL DEFAULT 0 CHECK (
        booked_balance_minor BETWEEN 0 AND 999999999999999999
    ),
    status text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((account_kind = 'CUSTOMER_DEPOSIT') = (customer_id IS NOT NULL))
);

CREATE TABLE app.operations (
    operation_id uuid PRIMARY KEY,
    operation_type text NOT NULL CHECK (operation_type IN ('RESERVE', 'RELEASE', 'POST', 'REVERSE')),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    resource_type text NOT NULL CHECK (resource_type IN ('HOLD', 'POSTING')),
    resource_id uuid NOT NULL,
    correlation_id uuid NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app.holds (
    hold_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES app.accounts(account_id),
    reserve_operation_id uuid NOT NULL UNIQUE REFERENCES app.operations(operation_id),
    amount_minor bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 999999999999999999),
    currency text NOT NULL CHECK (currency IN ('SGD', 'USD')),
    status text NOT NULL CHECK (status IN ('ACTIVE', 'CONSUMED', 'RELEASED')),
    expires_at timestamptz,
    consumed_by_operation_id uuid UNIQUE REFERENCES app.operations(operation_id),
    released_by_operation_id uuid UNIQUE REFERENCES app.operations(operation_id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'CONSUMED') = (consumed_by_operation_id IS NOT NULL)),
    CHECK ((status = 'RELEASED') = (released_by_operation_id IS NOT NULL)),
    CHECK (NOT (consumed_by_operation_id IS NOT NULL AND released_by_operation_id IS NOT NULL))
);

CREATE INDEX holds_active_account_idx ON app.holds(account_id) WHERE status = 'ACTIVE';

CREATE TABLE app.posting_journals (
    posting_id uuid PRIMARY KEY,
    operation_id uuid NOT NULL UNIQUE REFERENCES app.operations(operation_id),
    description text NOT NULL CHECK (length(description) BETWEEN 1 AND 256),
    reversal_of_posting_id uuid UNIQUE REFERENCES app.posting_journals(posting_id),
    committed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app.posting_entries (
    posting_id uuid NOT NULL REFERENCES app.posting_journals(posting_id),
    line_number smallint NOT NULL CHECK (line_number > 0),
    account_id uuid NOT NULL REFERENCES app.accounts(account_id),
    side text NOT NULL CHECK (side IN ('DEBIT', 'CREDIT')),
    amount_minor bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 999999999999999999),
    currency text NOT NULL CHECK (currency IN ('SGD', 'USD')),
    PRIMARY KEY (posting_id, line_number)
);

CREATE INDEX posting_entries_account_idx ON app.posting_entries(account_id, posting_id);

CREATE TABLE app.posting_events (
    event_id uuid PRIMARY KEY,
    event_type text NOT NULL CHECK (event_type = 'CoreBankingPostingCommitted'),
    schema_version integer NOT NULL CHECK (schema_version = 1),
    aggregate_type text NOT NULL CHECK (aggregate_type = 'CoreBankingPosting'),
    aggregate_id uuid NOT NULL UNIQUE REFERENCES app.posting_journals(posting_id),
    aggregate_version bigint NOT NULL CHECK (aggregate_version = 1),
    event_sequence bigint NOT NULL CHECK (event_sequence = 1),
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    correlation_id uuid NOT NULL,
    causation_id uuid NOT NULL,
    source text NOT NULL CHECK (source = 'core-banking-service'),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object')
);

CREATE TABLE app.posting_outbox (
    event_id uuid PRIMARY KEY REFERENCES app.posting_events(event_id),
    occurred_at timestamptz NOT NULL,
    published_at timestamptz,
    delivery_attempts integer NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
    last_error text,
    CHECK (published_at IS NULL OR last_error IS NULL)
);

CREATE INDEX posting_outbox_pending_idx ON app.posting_outbox(occurred_at, event_id)
WHERE published_at IS NULL;

CREATE FUNCTION app.reject_immutable_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER posting_journals_immutable
BEFORE UPDATE OR DELETE ON app.posting_journals
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();

CREATE TRIGGER posting_entries_immutable
BEFORE UPDATE OR DELETE ON app.posting_entries
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();

CREATE TRIGGER posting_events_immutable
BEFORE UPDATE OR DELETE ON app.posting_events
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();

