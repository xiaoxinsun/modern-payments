-- B03 event delivery: a global, gap-tolerant journal position for projection
-- rebuild watermarks and catch-up paging. Positions are allocated at insert, so
-- a reader must tolerate a lower position becoming visible later (the live
-- Kafka path delivers those events). The journal itself stays append-only.
ALTER TABLE app.domain_events ADD COLUMN journal_position bigint NOT NULL GENERATED ALWAYS AS IDENTITY;
ALTER TABLE app.domain_events ADD CONSTRAINT domain_events_journal_position_key UNIQUE (journal_position);
CREATE INDEX domain_events_aggregate_sequence_idx ON app.domain_events(aggregate_id, event_sequence);
