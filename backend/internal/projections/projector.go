package projections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/eventing"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

// PaymentConsumer is the live payment projection consumer's checkpoint scope.
const PaymentConsumer = "payment-projector"

// Incoming is one event offered to the projector. Event is nil when the record
// could not be decoded; Problem then says why.
type Incoming struct {
	Event    *contracts.Event
	Raw      []byte
	Problem  string
	Delivery eventing.Delivery
	Kafka    *KafkaPosition
}

type KafkaPosition struct {
	Topic         string
	Partition     int32
	Offset        int64
	HighWatermark int64
}

// Result counts per-generation outcomes. Projected lists events that were newly
// applied to the ACTIVE generation, with how they arrived.
type Result struct {
	Projected  int
	Duplicates int
	Buffered   int
	Poisoned   int
	Applied    []Applied
}

type Applied struct {
	Event    contracts.Event
	Delivery eventing.Delivery
}

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("GTB_DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse projections database configuration: %w", err)
	}
	config.MaxConns = 8
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	platformtelemetry.ConfigurePGX(config)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open projections database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping projections database: %w", err)
	}
	if err := platformtelemetry.RecordPGXStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("instrument projections database: %w", err)
	}
	return pool, nil
}

func (store *Store) Ready(ctx context.Context) error {
	return store.pool.Ping(ctx)
}

// ErrGenerationClosed means a targeted generation is no longer ACTIVE or
// BUILDING, for example because a rebuild switched or abandoned it.
var ErrGenerationClosed = errors.New("projection generation is not open")

// errPoison marks an event that can never be projected as delivered; it goes to
// the repair queue instead of blocking its partition.
type errPoison struct{ reason string }

func (err errPoison) Error() string { return err.reason }

// Process projects a batch in one transaction: deduplication, projection
// changes, gap buffering, repair entries and the Kafka checkpoints of the batch
// commit together, before the caller acknowledges anything. With generation nil
// the batch goes to every open generation (ACTIVE and any BUILDING shadow);
// otherwise only to that open generation.
func (store *Store) Process(ctx context.Context, consumer string, items []Incoming, generation *int32) (Result, error) {
	var result Result
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := projectiondb.New(tx)
	generations, err := queries.OpenGenerations(ctx)
	if err != nil {
		return result, err
	}
	var targets []projectiondb.OpenGenerationsRow
	for _, open := range generations {
		if generation == nil || open.Generation == *generation {
			targets = append(targets, open)
		}
	}
	if generation != nil && len(targets) == 0 {
		return result, fmt.Errorf("%w: %d", ErrGenerationClosed, *generation)
	}
	now := store.now().UTC()
	var lastEventAt *time.Time
	for _, item := range items {
		if item.Event == nil {
			if err := store.repair(ctx, queries, consumer, item, nil, item.Problem); err != nil {
				return result, err
			}
			result.Poisoned++
		} else if problem := checkEnvelope(*item.Event); problem != "" {
			if err := store.repair(ctx, queries, consumer, item, item.Event, problem); err != nil {
				return result, err
			}
			result.Poisoned++
		} else {
			for _, target := range targets {
				if err := store.project(ctx, tx, consumer, target, item, &result); err != nil {
					return result, err
				}
			}
			occurred := item.Event.OccurredAt
			lastEventAt = &occurred
		}
		if item.Kafka != nil {
			if err := queries.UpsertOffset(ctx, projectiondb.UpsertOffsetParams{
				Consumer: consumer, Topic: item.Kafka.Topic, Partition: item.Kafka.Partition,
				NextOffset: item.Kafka.Offset + 1, HighWatermark: max(item.Kafka.HighWatermark, item.Kafka.Offset+1),
				UpdatedAt: timestamp(now),
			}); err != nil {
				return result, err
			}
		}
	}
	if len(items) > 0 && items[0].Kafka != nil {
		if err := queries.TouchConsumer(ctx, projectiondb.TouchConsumerParams{
			Consumer: consumer, BrokerSeenAt: timestamp(now), LastEventAt: nullableTimestamp(lastEventAt),
		}); err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}

// project applies one event to one generation, then drains any buffered
// successors it unblocks.
func (store *Store) project(ctx context.Context, tx pgx.Tx, consumer string, target projectiondb.OpenGenerationsRow, item Incoming, result *Result) error {
	queries := projectiondb.New(tx)
	event := *item.Event
	aggregateID := mustUUID(event.AggregateID)
	if err := queries.LockAggregate(ctx, fmt.Sprintf("%d|%s", target.Generation, event.AggregateID)); err != nil {
		return err
	}
	duplicate, err := queries.InboxContains(ctx, projectiondb.InboxContainsParams{Generation: target.Generation, EventID: mustUUID(event.EventID)})
	if err != nil {
		return err
	}
	if duplicate {
		result.Duplicates++
		return nil
	}
	last, err := store.progress(ctx, queries, target.Generation, aggregateID)
	if err != nil {
		return err
	}
	switch {
	case event.EventSequence <= last:
		result.Poisoned++
		return store.repair(ctx, queries, consumer, item, &event, fmt.Sprintf("sequence %d already projected by a different event", event.EventSequence))
	case event.EventSequence > last+1:
		envelope, err := json.Marshal(event)
		if err != nil {
			return err
		}
		stored, err := queries.BufferEvent(ctx, projectiondb.BufferEventParams{
			Generation: target.Generation, EventID: mustUUID(event.EventID), AggregateID: aggregateID,
			EventSequence: event.EventSequence, Delivery: string(item.Delivery), Envelope: envelope,
			ReceivedAt: timestamp(store.now().UTC()),
		})
		if stored == 0 {
			result.Duplicates++ // already waiting in the buffer
		} else {
			result.Buffered++
		}
		return err
	}

	next := item
	for {
		applied, err := store.apply(ctx, tx, consumer, target.Generation, next)
		if err != nil {
			return err
		}
		if !applied {
			result.Poisoned++
			return nil
		}
		result.Projected++
		if target.Status == "ACTIVE" {
			result.Applied = append(result.Applied, Applied{Event: *next.Event, Delivery: next.Delivery})
		}
		successor, found, err := store.takeBuffered(ctx, queries, consumer, target.Generation, aggregateID, next.Event.EventSequence+1, result)
		if err != nil || !found {
			return err
		}
		next = successor
	}
}

// takeBuffered removes buffered events up to sequence and returns the one at
// sequence, if any. Earlier leftovers are duplicates or conflicting identities.
func (store *Store) takeBuffered(ctx context.Context, queries *projectiondb.Queries, consumer string, generation int32, aggregateID uuid.UUID, sequence int64, result *Result) (Incoming, bool, error) {
	rows, err := queries.TakePendingThrough(ctx, projectiondb.TakePendingThroughParams{
		Generation: generation, AggregateID: aggregateID, ThroughSequence: sequence,
	})
	if err != nil {
		return Incoming{}, false, err
	}
	var successor Incoming
	found := false
	for _, row := range rows {
		var event contracts.Event
		if err := json.Unmarshal(row.Envelope, &event); err != nil {
			return Incoming{}, false, err
		}
		candidate := Incoming{Event: &event, Raw: row.Envelope, Delivery: eventing.Delivery(row.Delivery)}
		if row.EventSequence == sequence && !found {
			successor, found = candidate, true
			continue
		}
		seen, err := queries.InboxContains(ctx, projectiondb.InboxContainsParams{Generation: generation, EventID: row.EventID})
		if err != nil {
			return Incoming{}, false, err
		}
		if seen {
			result.Duplicates++
			continue
		}
		result.Poisoned++
		if err := store.repair(ctx, queries, consumer, candidate, &event, fmt.Sprintf("sequence %d already projected by a different event", row.EventSequence)); err != nil {
			return Incoming{}, false, err
		}
	}
	return successor, found, nil
}

// apply writes the summary, timeline and inbox rows inside a savepoint. A
// payload the projection cannot represent, or a row the schema rejects, is
// rolled back to the savepoint and sent to the repair queue.
func (store *Store) apply(ctx context.Context, tx pgx.Tx, consumer string, generation int32, item Incoming) (bool, error) {
	event := *item.Event
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = savepoint.Rollback(ctx) }()
	err = store.write(ctx, projectiondb.New(savepoint), generation, item)
	var poison errPoison
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return true, savepoint.Commit(ctx)
	case errors.As(err, &poison):
	case errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")):
		poison = errPoison{reason: "projection rejected by schema: " + pgErr.Code}
	default:
		return false, err
	}
	if err := savepoint.Rollback(ctx); err != nil {
		return false, err
	}
	return false, store.repair(ctx, projectiondb.New(tx), consumer, item, &event, poison.reason)
}

type paymentPayload struct {
	PaymentID        value.ID     `json:"paymentId"`
	CustomerID       value.ID     `json:"customerId"`
	Product          string       `json:"product"`
	Amount           *value.Money `json:"amount"`
	LifecycleState   string       `json:"lifecycleState"`
	ExecutionOutcome string       `json:"executionOutcome"`
}

var (
	lifecycleStates = []string{"RECEIVED", "VALIDATING", "SCHEDULED", "REQUIRES_REVIEW", "ACCEPTED",
		"EXECUTING", "COMPLETED", "REJECTED", "CANCELLED", "RETURNED"}
	executionOutcomes = []string{"NOT_SUBMITTED", "PENDING", "CONFIRMED_SUCCESS", "CONFIRMED_FAILURE", "UNKNOWN"}
	products          = []string{"BOOK_TRANSFER", "DOMESTIC_INSTANT", "RTGS", "CROSS_BORDER"}
)

func (store *Store) write(ctx context.Context, queries *projectiondb.Queries, generation int32, item Incoming) error {
	event := *item.Event
	var payload paymentPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return errPoison{reason: "payload is not a payment event payload"}
	}
	if payload.PaymentID != event.AggregateID {
		return errPoison{reason: "payload paymentId does not match aggregateId"}
	}
	if !contains(lifecycleStates, payload.LifecycleState) || !contains(executionOutcomes, payload.ExecutionOutcome) {
		return errPoison{reason: "payload lacks a known lifecycleState and executionOutcome"}
	}
	now := timestamp(store.now().UTC())
	paymentID := mustUUID(event.AggregateID)
	if event.EventSequence == 1 {
		if event.EventType != "PaymentReceived" {
			return errPoison{reason: "first payment event must be PaymentReceived"}
		}
		if payload.CustomerID.Validate() != nil || !contains(products, payload.Product) || payload.Amount == nil || payload.Amount.AmountMinor < 1 {
			return errPoison{reason: "PaymentReceived lacks customerId, product or a positive amount"}
		}
		if err := queries.InsertSummary(ctx, projectiondb.InsertSummaryParams{
			Generation: generation, PaymentID: paymentID, CustomerID: mustUUID(payload.CustomerID),
			Product: payload.Product, AmountMinor: payload.Amount.AmountMinor, Currency: string(payload.Amount.Currency),
			LifecycleState: payload.LifecycleState, ExecutionOutcome: payload.ExecutionOutcome,
			ProjectedVersion: event.AggregateVersion, LastEventSequence: 1, ReceivedAt: timestamp(event.OccurredAt),
			LastEventAt: timestamp(event.OccurredAt), ProjectedAt: now,
		}); err != nil {
			return err
		}
	} else {
		updated, err := queries.AdvanceSummary(ctx, projectiondb.AdvanceSummaryParams{
			LifecycleState: payload.LifecycleState, ExecutionOutcome: payload.ExecutionOutcome,
			ProjectedVersion: event.AggregateVersion, EventSequence: event.EventSequence,
			LastEventAt: timestamp(event.OccurredAt), ProjectedAt: now, Generation: generation, PaymentID: paymentID,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return errPoison{reason: "summary is not at the preceding sequence"}
		}
	}
	if err := queries.InsertTimeline(ctx, projectiondb.InsertTimelineParams{
		Generation: generation, PaymentID: paymentID, EventSequence: event.EventSequence, EventID: mustUUID(event.EventID),
		EventType: event.EventType, AggregateVersion: event.AggregateVersion, LifecycleState: payload.LifecycleState,
		ExecutionOutcome: payload.ExecutionOutcome, OccurredAt: timestamp(event.OccurredAt),
		RecordedAt: timestamp(event.RecordedAt), CorrelationID: mustUUID(event.CorrelationID),
		CausationID: mustUUID(event.CausationID), ProjectedAt: now,
	}); err != nil {
		return err
	}
	return queries.InsertInbox(ctx, projectiondb.InsertInboxParams{
		Generation: generation, EventID: mustUUID(event.EventID), AggregateID: paymentID,
		EventSequence: event.EventSequence, Delivery: string(item.Delivery), ProcessedAt: now,
	})
}

func (store *Store) progress(ctx context.Context, queries *projectiondb.Queries, generation int32, paymentID uuid.UUID) (int64, error) {
	last, err := queries.SummaryProgress(ctx, projectiondb.SummaryProgressParams{Generation: generation, PaymentID: paymentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return last, err
}

func (store *Store) repair(ctx context.Context, queries *projectiondb.Queries, consumer string, item Incoming, event *contracts.Event, reason string) error {
	repairID, err := value.NewID()
	if err != nil {
		return err
	}
	params := projectiondb.InsertRepairParams{
		RepairID: mustUUID(repairID), Consumer: consumer, Reason: truncate(reason, 500),
		RecordValue: optionalText(truncate(strings.ToValidUTF8(string(item.Raw), ""), 64*1024)),
		FirstSeenAt: timestamp(store.now().UTC()),
	}
	if item.Kafka != nil {
		params.Topic, params.Partition, params.RecordOffset = &item.Kafka.Topic, &item.Kafka.Partition, &item.Kafka.Offset
	}
	if event != nil {
		if id, err := uuid.FromString(string(event.EventID)); err == nil {
			params.EventID = pgtype.UUID{Bytes: id, Valid: true}
		}
		if id, err := uuid.FromString(string(event.AggregateID)); err == nil {
			params.AggregateID = pgtype.UUID{Bytes: id, Valid: true}
		}
		params.EventSequence = &event.EventSequence
	}
	return queries.InsertRepair(ctx, params)
}

// checkEnvelope rejects events this consumer does not own or cannot identify.
func checkEnvelope(event contracts.Event) string {
	if err := event.Validate(); err != nil {
		return "invalid event envelope: " + err.Error()
	}
	if event.AggregateType != "Payment" {
		return "unexpected aggregate type " + truncate(event.AggregateType, 64)
	}
	return ""
}

// Decode turns a Kafka record value into an Incoming event; undecodable values
// are returned with a Problem for the repair queue.
func Decode(raw []byte, delivery eventing.Delivery, position *KafkaPosition) Incoming {
	item := Incoming{Raw: raw, Delivery: delivery, Kafka: position}
	var event contracts.Event
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil || decoder.More() {
		item.Problem = "undecodable event envelope"
		return item
	}
	item.Event = &event
	return item
}

func contains(values []string, candidate string) bool {
	for _, allowed := range values {
		if allowed == candidate {
			return true
		}
	}
	return false
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "")
}

func optionalText(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func mustUUID(id value.ID) uuid.UUID {
	parsed, err := uuid.FromString(string(id))
	if err != nil {
		panic(fmt.Sprintf("unvalidated identifier reached projection storage: %v", err))
	}
	return parsed
}

func timestamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}

func nullableTimestamp(at *time.Time) pgtype.Timestamptz {
	if at == nil {
		return pgtype.Timestamptz{}
	}
	return timestamp(*at)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
