package corebanking

import (
	"context"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/corebanking/storage/coredb"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/value"
)

// Deliver implements eventing.OutboxSource for the posting outbox. Rows are
// marked published only after Kafka acknowledges them, in the same transaction
// that holds their locks.
func (service *Service) Deliver(ctx context.Context, limit int, publish func(context.Context, []contracts.Event) []error) (eventing.DeliveryResult, error) {
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return eventing.DeliveryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := coredb.New(tx)
	rows, err := queries.LockPendingPostingOutbox(ctx, int32(limit))
	if err != nil || len(rows) == 0 {
		return eventing.DeliveryResult{}, err
	}
	events := make([]contracts.Event, len(rows))
	for index, row := range rows {
		events[index] = contracts.Event{
			EventID: value.ID(row.EventID.String()), EventType: row.EventType, SchemaVersion: int(row.SchemaVersion),
			AggregateType: row.AggregateType, AggregateID: value.ID(row.AggregateID.String()),
			AggregateVersion: row.AggregateVersion, EventSequence: row.EventSequence,
			OccurredAt: row.OccurredAt.Time.UTC(), RecordedAt: row.RecordedAt.Time.UTC(),
			CorrelationID: value.ID(row.CorrelationID.String()), CausationID: value.ID(row.CausationID.String()),
			Source: row.Source, Payload: row.Payload,
		}
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
		message := eventing.DeliveryError(publishErr)
		if err := queries.MarkPostingOutboxFailed(ctx, coredb.MarkPostingOutboxFailedParams{
			LastError: &message, EventID: rows[index].EventID,
		}); err != nil {
			return eventing.DeliveryResult{}, err
		}
	}
	if len(published) > 0 {
		if err := queries.MarkPostingOutboxPublished(ctx, coredb.MarkPostingOutboxPublishedParams{
			PublishedAt: pgtype.Timestamptz{Time: service.now().UTC(), Valid: true}, EventIds: published,
		}); err != nil {
			return eventing.DeliveryResult{}, err
		}
	}
	return result, tx.Commit(ctx)
}
