package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/storage/paymentdb"
	"gtb.local/backend/internal/platform/value"
)

// Deliver implements eventing.OutboxSource for the payment outbox. The row
// locks are held while Kafka acknowledges, so a concurrent relay skips them
// instead of publishing them twice; a crash still leaves them undelivered.
func (store *Store) Deliver(ctx context.Context, limit int, publish func(context.Context, []contracts.Event) []error) (eventing.DeliveryResult, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return eventing.DeliveryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := paymentdb.New(tx)
	rows, err := queries.LockPendingOutbox(ctx, int32(limit))
	if err != nil || len(rows) == 0 {
		return eventing.DeliveryResult{}, err
	}
	events := make([]contracts.Event, len(rows))
	for index, row := range rows {
		events[index] = eventFromColumns(row.EventID, row.EventType, row.SchemaVersion, row.AggregateType, row.AggregateID,
			row.AggregateVersion, row.EventSequence, row.OccurredAt, row.RecordedAt, row.CorrelationID, row.CausationID, row.Source, upcast(row.Payload, row.AggregateID, row.VersionLifecycleState, row.VersionExecutionOutcome))
	}
	results := publish(ctx, events)
	var published []uuid.UUID
	var result eventing.DeliveryResult
	for index, publishErr := range results {
		if publishErr == nil {
			published = append(published, rows[index].EventID)
			result.Published++
			continue
		}
		result.Failed++
		if err := queries.MarkOutboxFailed(ctx, paymentdb.MarkOutboxFailedParams{
			LastError: optional(eventing.DeliveryError(publishErr)), EventID: rows[index].EventID,
		}); err != nil {
			return eventing.DeliveryResult{}, err
		}
	}
	if len(published) > 0 {
		if err := queries.MarkOutboxPublished(ctx, paymentdb.MarkOutboxPublishedParams{
			PublishedAt: timestamp(store.now().UTC()), EventIds: published,
		}); err != nil {
			return eventing.DeliveryResult{}, err
		}
	}
	return result, tx.Commit(ctx)
}

func (store *Store) JournalWatermark(ctx context.Context) (application.JournalWatermark, error) {
	row, err := paymentdb.New(store.pool).JournalWatermark(ctx)
	if err != nil {
		return application.JournalWatermark{}, err
	}
	return application.JournalWatermark{Position: row.Watermark, Events: row.Events, Aggregates: row.Aggregates}, nil
}

// Journal pages events with afterPosition < position <= upToPosition in
// position order.
func (store *Store) Journal(ctx context.Context, afterPosition, upToPosition int64, limit int32) ([]application.JournalEntry, error) {
	rows, err := paymentdb.New(store.pool).ListJournal(ctx, paymentdb.ListJournalParams{
		AfterPosition: afterPosition, UpToPosition: upToPosition, PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	entries := make([]application.JournalEntry, len(rows))
	for index, row := range rows {
		entries[index] = application.JournalEntry{Position: row.JournalPosition, Event: eventFromColumns(row.EventID, row.EventType,
			row.SchemaVersion, row.AggregateType, row.AggregateID, row.AggregateVersion, row.EventSequence, row.OccurredAt,
			row.RecordedAt, row.CorrelationID, row.CausationID, row.Source, upcast(row.Payload, row.AggregateID, row.VersionLifecycleState, row.VersionExecutionOutcome))}
	}
	return entries, nil
}

// AggregateJournal returns one payment's events after a sequence, in order.
func (store *Store) AggregateJournal(ctx context.Context, paymentID value.ID, afterSequence int64, limit int32) ([]application.JournalEntry, error) {
	rows, err := paymentdb.New(store.pool).ListAggregateJournal(ctx, paymentdb.ListAggregateJournalParams{
		AggregateID: mustUUID(paymentID), AfterSequence: afterSequence, PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	entries := make([]application.JournalEntry, len(rows))
	for index, row := range rows {
		entries[index] = application.JournalEntry{Position: row.JournalPosition, Event: eventFromColumns(row.EventID, row.EventType,
			row.SchemaVersion, row.AggregateType, row.AggregateID, row.AggregateVersion, row.EventSequence, row.OccurredAt,
			row.RecordedAt, row.CorrelationID, row.CausationID, row.Source, upcast(row.Payload, row.AggregateID, row.VersionLifecycleState, row.VersionExecutionOutcome))}
	}
	return entries, nil
}

func eventFromColumns(eventID uuid.UUID, eventType string, schemaVersion int32, aggregateType string, aggregateID uuid.UUID,
	aggregateVersion, eventSequence int64, occurredAt, recordedAt pgtype.Timestamptz, correlationID, causationID uuid.UUID,
	source string, payload []byte) contracts.Event {
	return contracts.Event{
		EventID: valueID(eventID), EventType: eventType, SchemaVersion: int(schemaVersion), AggregateType: aggregateType,
		AggregateID: valueID(aggregateID), AggregateVersion: aggregateVersion, EventSequence: eventSequence,
		OccurredAt: utc(occurredAt), RecordedAt: utc(recordedAt), CorrelationID: valueID(correlationID),
		CausationID: valueID(causationID), Source: source, Payload: payload,
	}
}

// upcast completes payloads journaled before B03, when Apply did not yet stamp
// the resulting state: paymentId, lifecycleState and executionOutcome are
// filled from the version-history row committed in the same transaction. The
// stored journal is never rewritten, and present fields are left unchanged.
func upcast(payload []byte, aggregateID uuid.UUID, lifecycleState, executionOutcome string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return payload
	}
	missing := false
	for key, fill := range map[string]string{"paymentId": aggregateID.String(), "lifecycleState": lifecycleState, "executionOutcome": executionOutcome} {
		if _, present := fields[key]; !present {
			encoded, _ := json.Marshal(fill)
			fields[key], missing = encoded, true
		}
	}
	if !missing {
		return payload
	}
	upcasted, err := json.Marshal(fields)
	if err != nil {
		return payload
	}
	return upcasted
}

func utc(at pgtype.Timestamptz) time.Time {
	return at.Time.UTC()
}
