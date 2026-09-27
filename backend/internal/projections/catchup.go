package projections

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofrs/uuid/v5"

	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

// GapFiller resolves ordering gaps. Events buffered behind a missing sequence
// wait for GapTimeout (the missing event may still arrive from Kafka), or less
// when an aggregate buffers BufferLimit events; then the missing events are read
// from the payment journal and projected as REPLAY deliveries.
type GapFiller struct {
	Store       *Store
	Journal     Journal
	Logger      *slog.Logger
	Consumer    string
	GapTimeout  time.Duration
	BufferLimit int64
	Interval    time.Duration
}

func NewGapFiller(store *Store, journal Journal, logger *slog.Logger, consumer string) *GapFiller {
	return &GapFiller{
		Store: store, Journal: journal, Logger: logger, Consumer: consumer,
		GapTimeout: 5 * time.Second, BufferLimit: 100, Interval: time.Second,
	}
}

func (filler *GapFiller) Run(ctx context.Context) {
	ticker := time.NewTicker(filler.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if filled, err := filler.FillOnce(ctx, filler.GapTimeout, nil); err != nil && ctx.Err() == nil {
			filler.Logger.Warn("ordering-gap catch-up failed; events stay buffered", "error", err)
		} else if filled > 0 {
			filler.Logger.Info("ordering gaps filled from the payment journal", "aggregates", filled)
		}
	}
}

// FillOnce fills gaps buffered for at least minAge, optionally limited to one
// generation, and returns how many aggregates it caught up.
func (filler *GapFiller) FillOnce(ctx context.Context, minAge time.Duration, generation *int32) (int, error) {
	rows, err := projectiondb.New(filler.Store.pool).GapCandidates(ctx, projectiondb.GapCandidatesParams{
		OlderThan: timestamp(filler.Store.now().UTC().Add(-minAge)), BufferLimit: filler.BufferLimit, MaxAggregates: 50,
	})
	if err != nil {
		return 0, err
	}
	filled := 0
	for _, row := range rows {
		if generation != nil && row.Generation != *generation {
			continue
		}
		if err := filler.fill(ctx, row.Generation, row.AggregateID, row.ProjectedSequence); err != nil {
			return filled, err
		}
		filled++
	}
	return filled, nil
}

func (filler *GapFiller) fill(ctx context.Context, generation int32, aggregateID uuid.UUID, after int64) error {
	paymentID := value.ID(aggregateID.String())
	fetched := 0
	for {
		entries, err := filler.Journal.PaymentEvents(ctx, paymentID, after, 200)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			break
		}
		items := make([]Incoming, len(entries))
		for index, entry := range entries {
			event := entry.Event
			items[index] = Incoming{Event: &event, Delivery: eventing.DeliveryReplay}
		}
		if _, err := filler.Store.Process(ctx, filler.Consumer, items, &generation); err != nil {
			return err
		}
		fetched += len(entries)
		after = entries[len(entries)-1].Event.EventSequence
		if len(entries) < 200 {
			break
		}
	}
	// Every published event was committed to the journal first, so anything
	// still buffered behind the journal's end cannot be a real payment event.
	quarantined, err := filler.Store.quarantineBuffered(ctx, filler.Consumer, generation, aggregateID, after)
	if err != nil {
		return err
	}
	if quarantined > 0 {
		filler.Logger.Warn("buffered events absent from the payment journal moved to the repair queue",
			"payment_id", paymentID, "generation", generation, "events", quarantined)
	}
	filler.Logger.Info("ordering gap caught up from the journal", "payment_id", paymentID, "generation", generation, "journal_events", fetched)
	return nil
}

// quarantineBuffered moves buffered events beyond the journal's last sequence
// for an aggregate to the repair queue.
func (store *Store) quarantineBuffered(ctx context.Context, consumer string, generation int32, aggregateID uuid.UUID, journalEnd int64) (int, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := projectiondb.New(tx)
	if err := queries.LockAggregate(ctx, fmt.Sprintf("%d|%s", generation, aggregateID)); err != nil {
		return 0, err
	}
	last, err := store.progress(ctx, queries, generation, aggregateID)
	if err != nil {
		return 0, err
	}
	if last < journalEnd {
		// The journal is ahead of this generation; the next pass will project it.
		return 0, tx.Commit(ctx)
	}
	rows, err := queries.TakePendingThrough(ctx, projectiondb.TakePendingThroughParams{
		Generation: generation, AggregateID: aggregateID, ThroughSequence: value.MaxMinorUnits,
	})
	if err != nil {
		return 0, err
	}
	var result Result
	for _, row := range rows {
		item := Decode(row.Envelope, eventing.Delivery(row.Delivery), nil)
		seen, err := queries.InboxContains(ctx, projectiondb.InboxContainsParams{Generation: generation, EventID: row.EventID})
		if err != nil {
			return 0, err
		}
		if seen {
			continue
		}
		result.Poisoned++
		if err := store.repair(ctx, queries, consumer, item, item.Event, "buffered event is not in the payment journal"); err != nil {
			return 0, err
		}
	}
	return result.Poisoned, tx.Commit(ctx)
}
