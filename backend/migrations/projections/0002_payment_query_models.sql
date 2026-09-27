-- B03 payment query models. Every row is derived from payment domain events and
-- can be rebuilt from the payment journal; none of it authorizes money movement.
--
-- Projection tables are keyed by generation. Reads use the single ACTIVE
-- generation; a rebuild fills a BUILDING generation (the shadow tables) while the
-- live consumer writes to both, then one transaction switches ACTIVE.

CREATE TABLE app.projection_generations (
    generation integer PRIMARY KEY CHECK (generation >= 1),
    status text NOT NULL CHECK (status IN ('BUILDING', 'ACTIVE', 'RETIRED', 'FAILED')),
    journal_watermark bigint CHECK (journal_watermark >= 0),
    verification jsonb,
    created_at timestamptz NOT NULL,
    activated_at timestamptz,
    finished_at timestamptz,
    CHECK (status <> 'ACTIVE' OR (activated_at IS NOT NULL AND finished_at IS NULL))
);

CREATE UNIQUE INDEX projection_generations_one_active ON app.projection_generations ((true)) WHERE status = 'ACTIVE';
CREATE UNIQUE INDEX projection_generations_one_building ON app.projection_generations ((true)) WHERE status = 'BUILDING';

INSERT INTO app.projection_generations (generation, status, created_at, activated_at)
VALUES (1, 'ACTIVE', now(), now());

CREATE TABLE app.payment_summary (
    generation integer NOT NULL REFERENCES app.projection_generations(generation),
    payment_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    product text NOT NULL CHECK (product IN ('BOOK_TRANSFER', 'DOMESTIC_INSTANT', 'RTGS', 'CROSS_BORDER')),
    amount_minor bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 999999999999999999),
    currency text NOT NULL CHECK (currency IN ('SGD', 'USD')),
    lifecycle_state text NOT NULL,
    execution_outcome text NOT NULL,
    projected_version bigint NOT NULL CHECK (projected_version >= 1),
    last_event_sequence bigint NOT NULL CHECK (last_event_sequence >= 1),
    received_at timestamptz NOT NULL,
    last_event_at timestamptz NOT NULL,
    projected_at timestamptz NOT NULL,
    PRIMARY KEY (generation, payment_id)
);

CREATE INDEX payment_summary_received_idx ON app.payment_summary (generation, received_at DESC, payment_id DESC);
CREATE INDEX payment_summary_customer_idx ON app.payment_summary (generation, customer_id, received_at DESC, payment_id DESC);

CREATE TABLE app.payment_timeline (
    generation integer NOT NULL REFERENCES app.projection_generations(generation),
    payment_id uuid NOT NULL,
    event_sequence bigint NOT NULL CHECK (event_sequence >= 1),
    event_id uuid NOT NULL,
    event_type text NOT NULL,
    aggregate_version bigint NOT NULL CHECK (aggregate_version >= 1),
    lifecycle_state text NOT NULL,
    execution_outcome text NOT NULL,
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    correlation_id uuid NOT NULL,
    causation_id uuid NOT NULL,
    projected_at timestamptz NOT NULL,
    PRIMARY KEY (generation, payment_id, event_sequence),
    UNIQUE (generation, event_id)
);

-- Consumer deduplication: an event is projected at most once per generation.
CREATE TABLE app.inbox (
    generation integer NOT NULL REFERENCES app.projection_generations(generation),
    event_id uuid NOT NULL,
    aggregate_id uuid NOT NULL,
    event_sequence bigint NOT NULL,
    delivery text NOT NULL CHECK (delivery IN ('LIVE', 'REPLAY')),
    processed_at timestamptz NOT NULL,
    PRIMARY KEY (generation, event_id)
);

-- Ordering-gap buffer: events that arrived ahead of a missing sequence wait here
-- until the gap is filled from Kafka or from the journal.
CREATE TABLE app.pending_events (
    generation integer NOT NULL REFERENCES app.projection_generations(generation),
    event_id uuid NOT NULL,
    aggregate_id uuid NOT NULL,
    event_sequence bigint NOT NULL,
    delivery text NOT NULL CHECK (delivery IN ('LIVE', 'REPLAY')),
    envelope jsonb NOT NULL,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (generation, event_id)
);

CREATE INDEX pending_events_aggregate_idx ON app.pending_events (generation, aggregate_id, event_sequence);

-- Kafka checkpoints, committed with the projection changes they cover.
CREATE TABLE app.consumer_offsets (
    consumer text NOT NULL,
    topic text NOT NULL,
    partition integer NOT NULL CHECK (partition >= 0),
    next_offset bigint NOT NULL CHECK (next_offset >= 0),
    high_watermark bigint NOT NULL CHECK (high_watermark >= 0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, topic, partition)
);

-- Liveness of each consumer's broker connection, for freshness indicators.
CREATE TABLE app.consumer_status (
    consumer text PRIMARY KEY,
    broker_seen_at timestamptz NOT NULL,
    last_event_at timestamptz
);

-- Poison events: unreadable or unprojectable records, kept visible for repair
-- instead of blocking their partition.
CREATE TABLE app.repair_queue (
    repair_id uuid PRIMARY KEY,
    consumer text NOT NULL,
    topic text,
    partition integer,
    record_offset bigint,
    event_id uuid,
    aggregate_id uuid,
    event_sequence bigint,
    reason text NOT NULL,
    record_value text,
    status text NOT NULL CHECK (status IN ('OPEN', 'RESOLVED')),
    first_seen_at timestamptz NOT NULL,
    UNIQUE (consumer, topic, partition, record_offset),
    UNIQUE (consumer, event_id)
);

-- query-api's login (provisioned by make db-provision) reads query models and
-- delivery health only; raw poison records and the inbox stay private.
GRANT USAGE ON SCHEMA app TO query_reader;
GRANT SELECT ON app.projection_generations, app.payment_summary, app.payment_timeline,
    app.consumer_offsets, app.consumer_status TO query_reader;
GRANT SELECT (generation, aggregate_id, event_sequence, received_at) ON app.pending_events TO query_reader;
GRANT SELECT (repair_id, consumer, event_id, aggregate_id, event_sequence, reason, status, first_seen_at)
    ON app.repair_queue TO query_reader;
