package projections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

// Rebuilder rebuilds the payment projections into a shadow generation from the
// payment journal. It runs beside the live consumer, which projects every event
// it consumes into both the ACTIVE and the BUILDING generation:
//
//  1. create generation G+1 (BUILDING); from now on live events reach it;
//  2. read the journal watermark W and project every journal event up to W into
//     G+1 as REPLAY deliveries (events above W, or committed late below it,
//     arrive live; the inbox absorbs overlaps and the gap buffer reorders);
//  3. settle ordering gaps, then verify G+1 against the journal: payment and
//     event counts at W, no buffered gaps, and sampled timelines identical;
//  4. switch ACTIVE to G+1 in one transaction and delete the retired rows.
//
// Replay deliveries never reach effectful consumers, so a rebuild cannot move
// money or send notifications.
type Rebuilder struct {
	Store         *Store
	Journal       Journal
	Filler        *GapFiller
	Logger        *slog.Logger
	Consumer      string
	PageSize      int
	SampleSize    int32
	SettleTimeout time.Duration
}

type RebuildReport struct {
	Generation        int32            `json:"generation"`
	Watermark         JournalWatermark `json:"watermark"`
	ReplayedEvents    int              `json:"replayedEvents"`
	ProjectedPayments int64            `json:"projectedPayments"`
	ProjectedEvents   int64            `json:"projectedEvents"`
	SampledTimelines  int              `json:"sampledTimelines"`
	Problems          []string         `json:"problems"`
	Activated         bool             `json:"activated"`
	RetiredGeneration *int32           `json:"retiredGeneration,omitempty"`
	Duration          string           `json:"duration"`
}

var ErrRebuildVerification = errors.New("rebuilt projection does not match the payment journal")

func NewRebuilder(store *Store, journal Journal, filler *GapFiller, logger *slog.Logger, consumer string) *Rebuilder {
	return &Rebuilder{
		Store: store, Journal: journal, Filler: filler, Logger: logger, Consumer: consumer,
		PageSize: 500, SampleSize: 20, SettleTimeout: time.Minute,
	}
}

func (rebuilder *Rebuilder) Run(ctx context.Context) (RebuildReport, error) {
	started := time.Now()
	report := RebuildReport{Problems: []string{}}
	generation, err := rebuilder.Store.startGeneration(ctx)
	if err != nil {
		return report, err
	}
	report.Generation = generation
	rebuilder.Logger.Info("projection rebuild started", "generation", generation)
	if err := rebuilder.fill(ctx, &report); err != nil {
		// A failed rebuild must not leave a BUILDING generation for live writes.
		rebuilder.fail(context.WithoutCancel(ctx), &report, err.Error())
		return report, err
	}
	if err := rebuilder.verify(ctx, &report); err != nil {
		rebuilder.fail(context.WithoutCancel(ctx), &report, err.Error())
		return report, err
	}
	if len(report.Problems) > 0 {
		rebuilder.fail(ctx, &report, "")
		return report, ErrRebuildVerification
	}
	retired, err := rebuilder.Store.activate(ctx, generation, report)
	if err != nil {
		return report, err
	}
	report.Activated, report.RetiredGeneration = true, retired
	report.Duration = time.Since(started).Round(time.Millisecond).String()
	rebuilder.Logger.Info("projection rebuild activated", "generation", generation, "retired", retired,
		"payments", report.ProjectedPayments, "events", report.ProjectedEvents)
	return report, nil
}

func (rebuilder *Rebuilder) fill(ctx context.Context, report *RebuildReport) error {
	watermark, err := rebuilder.Journal.Watermark(ctx)
	if err != nil {
		return err
	}
	report.Watermark = watermark
	if err := projectiondb.New(rebuilder.Store.pool).SetWatermark(ctx, projectiondb.SetWatermarkParams{
		Generation: report.Generation, JournalWatermark: &watermark.Position,
	}); err != nil {
		return err
	}
	for after := int64(0); after < watermark.Position; {
		entries, err := rebuilder.Journal.Page(ctx, after, watermark.Position, rebuilder.PageSize)
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
		if _, err := rebuilder.Store.Process(ctx, rebuilder.Consumer, items, &report.Generation); err != nil {
			return err
		}
		report.ReplayedEvents += len(entries)
		after = entries[len(entries)-1].Position
	}
	// Live events that overtook the replay wait in the gap buffer; settle them.
	deadline := time.Now().Add(rebuilder.SettleTimeout)
	for {
		counts, err := projectiondb.New(rebuilder.Store.pool).GenerationCounts(ctx, report.Generation)
		if err != nil {
			return err
		}
		if counts.Buffered == 0 || time.Now().After(deadline) {
			return nil
		}
		if _, err := rebuilder.Filler.FillOnce(ctx, 0, &report.Generation); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (rebuilder *Rebuilder) verify(ctx context.Context, report *RebuildReport) error {
	queries := projectiondb.New(rebuilder.Store.pool)
	counts, err := queries.GenerationCounts(ctx, report.Generation)
	if err != nil {
		return err
	}
	report.ProjectedPayments, report.ProjectedEvents = counts.Payments, counts.Events
	// Live events above the watermark may already be present, so the rebuilt
	// generation must hold at least the journal's counts at the watermark.
	if counts.Payments < report.Watermark.Payments {
		report.Problems = append(report.Problems, fmt.Sprintf("projected %d payments; journal has %d at the watermark", counts.Payments, report.Watermark.Payments))
	}
	if counts.Events < report.Watermark.Events {
		report.Problems = append(report.Problems, fmt.Sprintf("projected %d events; journal has %d at the watermark", counts.Events, report.Watermark.Events))
	}
	if counts.Buffered > 0 {
		report.Problems = append(report.Problems, fmt.Sprintf("%d events still wait behind ordering gaps", counts.Buffered))
	}
	sample, err := queries.SampleAggregates(ctx, projectiondb.SampleAggregatesParams{Generation: report.Generation, SampleSize: rebuilder.SampleSize})
	if err != nil {
		return err
	}
	for _, paymentID := range sample {
		journal, err := rebuilder.Journal.PaymentEvents(ctx, value.ID(paymentID.String()), 0, 500)
		if err != nil {
			return err
		}
		projected, err := queries.GenerationTimeline(ctx, projectiondb.GenerationTimelineParams{Generation: report.Generation, PaymentID: paymentID})
		if err != nil {
			return err
		}
		if problem := compareTimeline(journal, projected, report.Watermark.Position); problem != "" {
			report.Problems = append(report.Problems, fmt.Sprintf("payment %s: %s", paymentID, problem))
		}
		report.SampledTimelines++
	}
	return nil
}

// compareTimeline requires the projected timeline to be a prefix-identical copy
// of the journal that covers every event at or below the watermark.
func compareTimeline(journal []JournalEntry, projected []projectiondb.GenerationTimelineRow, watermark int64) string {
	required := 0
	for index, entry := range journal {
		if entry.Position <= watermark {
			required = index + 1
		}
	}
	if len(projected) < required {
		return fmt.Sprintf("timeline has %d events; journal has %d at the watermark", len(projected), required)
	}
	for index := 0; index < len(projected) && index < len(journal); index++ {
		event, row := journal[index].Event, projected[index]
		if string(event.EventID) != row.EventID.String() || event.EventSequence != row.EventSequence ||
			event.EventType != row.EventType || event.AggregateVersion != row.AggregateVersion {
			return fmt.Sprintf("timeline differs from the journal at sequence %d", event.EventSequence)
		}
	}
	if len(projected) > len(journal) {
		return fmt.Sprintf("timeline has %d events the journal does not", len(projected)-len(journal))
	}
	return ""
}

func (rebuilder *Rebuilder) fail(ctx context.Context, report *RebuildReport, reason string) {
	if reason != "" {
		report.Problems = append(report.Problems, reason)
	}
	if err := rebuilder.Store.closeGeneration(ctx, report.Generation, *report); err != nil {
		rebuilder.Logger.Error("failed rebuild generation could not be closed", "generation", report.Generation, "error", err)
	}
	rebuilder.Logger.Warn("projection rebuild failed; the active generation is unchanged", "generation", report.Generation, "problems", report.Problems)
}

func (store *Store) startGeneration(ctx context.Context) (int32, error) {
	generation, err := projectiondb.New(store.pool).StartGeneration(ctx, timestamp(store.now().UTC()))
	if isUniqueViolation(err) {
		return 0, errors.New("another rebuild is BUILDING; wait for it, or run `event-projector rebuild -abandon-building` if it crashed")
	}
	return generation, err
}

// AbandonBuilding marks a crashed rebuild's generation FAILED and deletes its
// rows. It returns the abandoned generation, if any.
func (store *Store) AbandonBuilding(ctx context.Context) (*int32, error) {
	generations, err := projectiondb.New(store.pool).OpenGenerations(ctx)
	if err != nil {
		return nil, err
	}
	for _, open := range generations {
		if open.Status == "BUILDING" {
			generation := open.Generation
			return &generation, store.closeGeneration(ctx, generation, RebuildReport{Generation: generation, Problems: []string{"abandoned"}})
		}
	}
	return nil, nil
}

// activate switches reads atomically: queries always join the single ACTIVE
// generation, so they see the old or the new projection, never a mixture.
func (store *Store) activate(ctx context.Context, generation int32, report RebuildReport) (*int32, error) {
	verification, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	now := timestamp(store.now().UTC())
	var retired *int32
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		queries := projectiondb.New(tx)
		previous, err := queries.RetireActive(ctx, now)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			retired = &previous
		}
		updated, err := queries.ActivateGeneration(ctx, projectiondb.ActivateGenerationParams{
			ActivatedAt: now, Verification: verification, Generation: generation,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("%w: %d", ErrGenerationClosed, generation)
		}
		return nil
	})
	if err != nil || retired == nil {
		return retired, err
	}
	return retired, store.deleteGenerationRows(ctx, *retired)
}

func (store *Store) closeGeneration(ctx context.Context, generation int32, report RebuildReport) error {
	verification, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := projectiondb.New(store.pool).FailGeneration(ctx, projectiondb.FailGenerationParams{
		FinishedAt: timestamp(store.now().UTC()), Verification: verification, Generation: generation,
	}); err != nil {
		return err
	}
	return store.deleteGenerationRows(ctx, generation)
}

func (store *Store) deleteGenerationRows(ctx context.Context, generation int32) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		queries := projectiondb.New(tx)
		for _, remove := range []func(context.Context, int32) error{
			queries.DeleteGenerationPending, queries.DeleteGenerationInbox, queries.DeleteGenerationTimeline, queries.DeleteGenerationSummary,
		} {
			if err := remove(ctx, generation); err != nil {
				return err
			}
		}
		return nil
	})
}
