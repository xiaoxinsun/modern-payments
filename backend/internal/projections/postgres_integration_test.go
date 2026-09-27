package projections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/value"
)

// These tests switch projection generations, so they run against a scratch
// projections database (make eventing-integration), never the shared one.
func openScratch(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("GTB_PROJECTIONS_SCRATCH_DATABASE_URL")
	if url == "" {
		t.Skip("GTB_PROJECTIONS_SCRATCH_DATABASE_URL is not set")
	}
	pool, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

func testID(t *testing.T) value.ID {
	t.Helper()
	id, err := value.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

// paymentEvents builds a received payment followed by lifecycle transitions,
// shaped like the payment service's journal.
func paymentEvents(t *testing.T, paymentID value.ID, count int) []contracts.Event {
	t.Helper()
	states := []string{"RECEIVED", "VALIDATING", "ACCEPTED", "EXECUTING", "COMPLETED"}
	types := []string{"PaymentReceived", "PaymentValidationStarted", "PaymentAccepted", "PaymentExecutionStarted", "PaymentCompleted"}
	customer, correlation := testID(t), testID(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	events := make([]contracts.Event, count)
	for index := range count {
		payload := map[string]any{"paymentId": paymentID, "lifecycleState": states[index], "executionOutcome": "NOT_SUBMITTED"}
		if index == 0 {
			payload["customerId"], payload["product"] = customer, "BOOK_TRANSFER"
			payload["amount"] = map[string]string{"amountMinor": "2500", "currency": "SGD"}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		events[index] = contracts.Event{
			EventID: testID(t), EventType: types[index], SchemaVersion: 1, AggregateType: "Payment", AggregateID: paymentID,
			AggregateVersion: int64(index + 1), EventSequence: int64(index + 1), OccurredAt: at.Add(time.Duration(index) * time.Millisecond),
			RecordedAt: at, CorrelationID: correlation, CausationID: correlation, Source: "payment-api", Payload: encoded,
		}
	}
	return events
}

func deliveries(delivery eventing.Delivery, events ...contracts.Event) []Incoming {
	items := make([]Incoming, len(events))
	for index := range events {
		event := events[index]
		raw, _ := json.Marshal(event)
		items[index] = Incoming{Event: &event, Raw: raw, Delivery: delivery}
	}
	return items
}

func process(t *testing.T, store *Store, items []Incoming, generation *int32) Result {
	t.Helper()
	result, err := store.Process(context.Background(), PaymentConsumer, items, generation)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func activeGeneration(t *testing.T, store *Store) int32 {
	t.Helper()
	var generation int32
	if err := store.pool.QueryRow(context.Background(), `SELECT generation FROM app.projection_generations WHERE status = 'ACTIVE'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

type summaryState struct {
	Version, LastSequence int64
	Lifecycle             string
	Timeline              []int64
	Inbox                 map[string]string
	Buffered              int
}

func state(t *testing.T, store *Store, generation int32, paymentID value.ID) summaryState {
	t.Helper()
	ctx := context.Background()
	result := summaryState{Inbox: map[string]string{}}
	err := store.pool.QueryRow(ctx, `SELECT projected_version, last_event_sequence, lifecycle_state FROM app.payment_summary
		WHERE generation = $1 AND payment_id = $2`, generation, string(paymentID)).Scan(&result.Version, &result.LastSequence, &result.Lifecycle)
	if err != nil && err.Error() != "no rows in result set" {
		t.Fatal(err)
	}
	rows, err := store.pool.Query(ctx, `SELECT event_sequence FROM app.payment_timeline WHERE generation = $1 AND payment_id = $2 ORDER BY event_sequence`, generation, string(paymentID))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		result.Timeline = append(result.Timeline, sequence)
	}
	rows, err = store.pool.Query(ctx, `SELECT event_id::text, delivery FROM app.inbox WHERE generation = $1 AND aggregate_id = $2`, generation, string(paymentID))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, delivery string
		if err := rows.Scan(&id, &delivery); err != nil {
			t.Fatal(err)
		}
		result.Inbox[id] = delivery
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM app.pending_events WHERE generation = $1 AND aggregate_id = $2`, generation, string(paymentID)).Scan(&result.Buffered); err != nil {
		t.Fatal(err)
	}
	return result
}

func repairReasons(t *testing.T, store *Store, eventIDs ...value.ID) map[value.ID]string {
	t.Helper()
	reasons := map[value.ID]string{}
	for _, id := range eventIDs {
		var reason string
		if err := store.pool.QueryRow(context.Background(), `SELECT reason FROM app.repair_queue WHERE event_id = $1`, string(id)).Scan(&reason); err == nil {
			reasons[id] = reason
		}
	}
	return reasons
}

func TestScratchProjectorDeduplicatesAndCheckpointsAtomically(t *testing.T) {
	store := openScratch(t)
	generation := activeGeneration(t, store)
	paymentID := testID(t)
	events := paymentEvents(t, paymentID, 3)
	topic := "test." + string(testID(t))
	batch := deliveries(eventing.DeliveryLive, events[:2]...)
	for index := range batch {
		batch[index].Kafka = &KafkaPosition{Topic: topic, Partition: 0, Offset: int64(10 + index), HighWatermark: 12}
	}
	if result := process(t, store, batch, nil); result.Projected != 2 || len(result.Applied) != 2 {
		t.Fatalf("first delivery: %+v", result)
	}
	// Redelivery (lost acknowledgement, relay republish) changes nothing.
	if result := process(t, store, batch, nil); result.Projected != 0 || result.Duplicates != 2 {
		t.Fatalf("redelivery: %+v", result)
	}
	got := state(t, store, generation, paymentID)
	if got.Version != 2 || !slices.Equal(got.Timeline, []int64{1, 2}) || len(got.Inbox) != 2 {
		t.Fatalf("after redelivery: %+v", got)
	}
	var next, watermark int64
	if err := store.pool.QueryRow(context.Background(), `SELECT next_offset, high_watermark FROM app.consumer_offsets WHERE consumer = $1 AND topic = $2 AND partition = 0`,
		PaymentConsumer, topic).Scan(&next, &watermark); err != nil || next != 12 || watermark != 12 {
		t.Fatalf("checkpoint: next %d watermark %d: %v", next, watermark, err)
	}
	// A batch whose checkpoint cannot be written commits none of its projection
	// changes: projection and checkpoint are one transaction.
	failing := deliveries(eventing.DeliveryLive, events[2])
	failing[0].Kafka = &KafkaPosition{Topic: topic, Partition: -1, Offset: 12, HighWatermark: 13}
	if _, err := store.Process(context.Background(), PaymentConsumer, failing, nil); err == nil {
		t.Fatal("expected the invalid checkpoint to fail the batch")
	}
	if got := state(t, store, generation, paymentID); got.Version != 2 || len(got.Timeline) != 2 {
		t.Fatalf("failed batch left projection changes: %+v", got)
	}
	failing[0].Kafka.Partition = 0
	if result := process(t, store, failing, nil); result.Projected != 1 {
		t.Fatalf("retry: %+v", result)
	}
}

func TestScratchProjectorBuffersOrderingGapsAndDrains(t *testing.T) {
	store := openScratch(t)
	generation := activeGeneration(t, store)
	paymentID := testID(t)
	events := paymentEvents(t, paymentID, 4)
	if result := process(t, store, deliveries(eventing.DeliveryLive, events[3], events[1], events[2]), nil); result.Buffered != 3 || result.Projected != 0 {
		t.Fatalf("out of order: %+v", result)
	}
	if got := state(t, store, generation, paymentID); got.Buffered != 3 || len(got.Timeline) != 0 {
		t.Fatalf("buffered: %+v", got)
	}
	if result := process(t, store, deliveries(eventing.DeliveryLive, events[1]), nil); result.Buffered != 0 || result.Duplicates != 1 {
		t.Fatalf("duplicate of a buffered event must not buffer twice: %+v", result)
	}
	if result := process(t, store, deliveries(eventing.DeliveryLive, events[0]), nil); result.Projected != 4 {
		t.Fatalf("filling the gap drains the buffer: %+v", result)
	}
	got := state(t, store, generation, paymentID)
	if got.Version != 4 || got.Lifecycle != "EXECUTING" || !slices.Equal(got.Timeline, []int64{1, 2, 3, 4}) || got.Buffered != 0 {
		t.Fatalf("drained: %+v", got)
	}
}

func TestScratchProjectorQuarantinesPoisonEvents(t *testing.T) {
	store := openScratch(t)
	generation := activeGeneration(t, store)
	topic := "test." + string(testID(t))

	undecodable := Decode([]byte(`{"eventId":`), eventing.DeliveryLive, &KafkaPosition{Topic: topic, Partition: 1, Offset: 5, HighWatermark: 6})
	valid := paymentEvents(t, testID(t), 2)
	wrongType := valid[1]
	wrongType.EventID, wrongType.AggregateType = testID(t), "CoreBankingPosting"
	orphan := paymentEvents(t, testID(t), 2)
	orphan[0].EventType = "PaymentAccepted"
	badState := paymentEvents(t, testID(t), 1)[0]
	badState.Payload = json.RawMessage(`{"paymentId":"` + string(badState.AggregateID) + `","lifecycleState":"TELEPORTED","executionOutcome":"NOT_SUBMITTED"}`)
	conflict := valid[0]
	conflict.EventID = testID(t)

	items := append([]Incoming{undecodable}, deliveries(eventing.DeliveryLive, valid[0], wrongType, orphan[0], orphan[1], badState, conflict)...)
	result := process(t, store, items, nil)
	if result.Poisoned != 5 || result.Projected != 1 || result.Buffered != 1 {
		t.Fatalf("poison batch: %+v", result)
	}
	reasons := repairReasons(t, store, wrongType.EventID, orphan[0].EventID, badState.EventID, conflict.EventID)
	if len(reasons) != 4 {
		t.Fatalf("repair queue: %v", reasons)
	}
	var reason string
	var offset int64
	if err := store.pool.QueryRow(context.Background(), `SELECT reason, record_offset FROM app.repair_queue WHERE topic = $1 AND partition = 1`, topic).Scan(&reason, &offset); err != nil || offset != 5 {
		t.Fatalf("undecodable record not queued: %v", err)
	}
	var next int64
	if err := store.pool.QueryRow(context.Background(), `SELECT next_offset FROM app.consumer_offsets WHERE consumer = $1 AND topic = $2 AND partition = 1`,
		PaymentConsumer, topic).Scan(&next); err != nil || next != 6 {
		t.Fatalf("poison must not block its partition: next %d %v", next, err)
	}
	// The orphan's successor waits visibly behind the quarantined first event.
	if got := state(t, store, generation, orphan[1].AggregateID); got.Buffered != 1 {
		t.Fatalf("orphan successor: %+v", got)
	}
	if got := state(t, store, generation, valid[0].AggregateID); got.Version != 1 || len(got.Timeline) != 1 {
		t.Fatalf("the valid event beside the poison ones: %+v", got)
	}
}

// fakeJournal is an in-memory payment journal with positions.
type fakeJournal struct {
	mu      sync.Mutex
	entries []JournalEntry
	onPage  func()
}

func (journal *fakeJournal) append(events ...contracts.Event) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for _, event := range events {
		journal.entries = append(journal.entries, JournalEntry{Position: int64(len(journal.entries) + 1), Event: event})
	}
}

func (journal *fakeJournal) Watermark(context.Context) (JournalWatermark, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	payments := map[value.ID]bool{}
	for _, entry := range journal.entries {
		payments[entry.Event.AggregateID] = true
	}
	return JournalWatermark{Position: int64(len(journal.entries)), Events: int64(len(journal.entries)), Payments: int64(len(payments))}, nil
}

func (journal *fakeJournal) Page(_ context.Context, after, upTo int64, limit int) ([]JournalEntry, error) {
	if journal.onPage != nil {
		hook := journal.onPage
		journal.onPage = nil
		hook()
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	var page []JournalEntry
	for _, entry := range journal.entries {
		if entry.Position > after && entry.Position <= upTo && len(page) < limit {
			page = append(page, entry)
		}
	}
	return page, nil
}

func (journal *fakeJournal) PaymentEvents(_ context.Context, paymentID value.ID, after int64, limit int) ([]JournalEntry, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	var page []JournalEntry
	for _, entry := range journal.entries {
		if entry.Event.AggregateID == paymentID && entry.Event.EventSequence > after && len(page) < limit {
			page = append(page, entry)
		}
	}
	return page, nil
}

func TestScratchGapFillerCatchesUpFromJournal(t *testing.T) {
	store := openScratch(t)
	generation := activeGeneration(t, store)
	journal := &fakeJournal{}
	filler := NewGapFiller(store, journal, testLogger, PaymentConsumer)

	paymentID := testID(t)
	events := paymentEvents(t, paymentID, 3)
	journal.append(events...)
	process(t, store, deliveries(eventing.DeliveryLive, events[2]), nil)
	// Too young to fill: the missing events may still arrive from Kafka.
	if _, err := filler.FillOnce(context.Background(), time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if got := state(t, store, generation, paymentID); got.Buffered != 1 {
		t.Fatalf("young gap was filled early: %+v", got)
	}
	if _, err := filler.FillOnce(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	got := state(t, store, generation, paymentID)
	if got.Version != 3 || got.Buffered != 0 || !slices.Equal(got.Timeline, []int64{1, 2, 3}) {
		t.Fatalf("after catch-up: %+v", got)
	}
	want := map[string]string{string(events[0].EventID): "REPLAY", string(events[1].EventID): "REPLAY", string(events[2].EventID): "LIVE"}
	for id, delivery := range want {
		if got.Inbox[id] != delivery {
			t.Fatalf("inbox delivery for %s: %q, want %q", id, got.Inbox[id], delivery)
		}
	}

	// A buffered event the journal does not contain cannot be real: repair it.
	strayID := testID(t)
	stray := paymentEvents(t, strayID, 3)
	journal.append(stray[0])
	process(t, store, deliveries(eventing.DeliveryLive, stray[0], stray[2]), nil)
	if _, err := filler.FillOnce(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := state(t, store, generation, strayID); got.Buffered != 0 || got.Version != 1 {
		t.Fatalf("stray: %+v", got)
	}
	if reasons := repairReasons(t, store, stray[2].EventID); reasons[stray[2].EventID] != "buffered event is not in the payment journal" {
		t.Fatalf("stray repair: %v", reasons)
	}
}

func TestScratchRebuildSwitchesGenerationsWhileLiveEventsArrive(t *testing.T) {
	store := openScratch(t)
	ctx := context.Background()
	journal := &fakeJournal{}
	filler := NewGapFiller(store, journal, testLogger, PaymentConsumer)
	rebuilder := NewRebuilder(store, journal, filler, testLogger, PaymentConsumer)
	rebuilder.PageSize = 2

	first, second := testID(t), testID(t)
	firstEvents, secondEvents := paymentEvents(t, first, 3), paymentEvents(t, second, 4)
	journal.append(firstEvents...)
	journal.append(secondEvents[:3]...)
	process(t, store, deliveries(eventing.DeliveryLive, append(slices.Clone(firstEvents), secondEvents[:3]...)...), nil)
	old := activeGeneration(t, store)

	// While the rebuild pages the journal, a new event commits and is consumed
	// live: it lands in both generations, ahead of the shadow's replay.
	journal.onPage = func() {
		journal.append(secondEvents[3])
		process(t, store, deliveries(eventing.DeliveryLive, secondEvents[3]), nil)
	}
	report, err := rebuilder.Run(ctx)
	if err != nil {
		t.Fatalf("rebuild: %v %+v", err, report)
	}
	if !report.Activated || report.RetiredGeneration == nil || *report.RetiredGeneration != old || report.Generation != old+1 {
		t.Fatalf("report: %+v", report)
	}
	if report.Watermark.Events != 6 || report.ProjectedEvents != 7 || report.SampledTimelines != 2 || len(report.Problems) != 0 {
		t.Fatalf("verification: %+v", report)
	}
	if active := activeGeneration(t, store); active != report.Generation {
		t.Fatalf("active generation %d, want %d", active, report.Generation)
	}
	for paymentID, version := range map[value.ID]int64{first: 3, second: 4} {
		got := state(t, store, report.Generation, paymentID)
		if got.Version != version || got.Buffered != 0 || int64(len(got.Timeline)) != version {
			t.Fatalf("%s in the new generation: %+v", paymentID, got)
		}
	}
	if got := state(t, store, old, first); len(got.Timeline) != 0 || len(got.Inbox) != 0 {
		t.Fatalf("retired generation rows remain: %+v", got)
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM app.projection_generations WHERE generation = $1`, old).Scan(&status); err != nil || status != "RETIRED" {
		t.Fatalf("old generation status %q: %v", status, err)
	}
	// Live delivery continues into the new generation only.
	third := paymentEvents(t, testID(t), 1)
	if result := process(t, store, deliveries(eventing.DeliveryLive, third...), nil); result.Projected != 1 {
		t.Fatalf("live after switch: %+v", result)
	}
}

func TestScratchFailedRebuildKeepsTheActiveGeneration(t *testing.T) {
	store := openScratch(t)
	ctx := context.Background()
	active := activeGeneration(t, store)
	journal := &fakeJournal{}
	healthy := paymentEvents(t, testID(t), 2)
	broken := paymentEvents(t, testID(t), 1)
	broken[0].EventType = "PaymentAccepted" // the journal holds a fact the projection cannot represent
	journal.append(healthy...)
	journal.append(broken...)
	rebuilder := NewRebuilder(store, journal, NewGapFiller(store, journal, testLogger, PaymentConsumer), testLogger, PaymentConsumer)
	report, err := rebuilder.Run(ctx)
	if !errors.Is(err, ErrRebuildVerification) || report.Activated || len(report.Problems) == 0 {
		t.Fatalf("expected a failed verification: %v %+v", err, report)
	}
	if got := activeGeneration(t, store); got != active {
		t.Fatalf("active generation changed to %d", got)
	}
	var status string
	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM app.inbox WHERE generation = $1) FROM app.projection_generations WHERE generation = $1`,
		report.Generation).Scan(&status, &rows); err != nil || status != "FAILED" || rows != 0 {
		t.Fatalf("failed generation: %q rows %d: %v", status, rows, err)
	}

	// A crashed rebuild leaves BUILDING; a new one refuses until abandoned.
	building, err := store.startGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.startGeneration(ctx); err == nil {
		t.Fatal("two rebuilds may not build at once")
	}
	abandoned, err := store.AbandonBuilding(ctx)
	if err != nil || abandoned == nil || *abandoned != building {
		t.Fatalf("abandon: %v %v", abandoned, err)
	}
	if _, err := store.Process(ctx, PaymentConsumer, deliveries(eventing.DeliveryReplay, healthy[0]), &building); !errors.Is(err, ErrGenerationClosed) {
		t.Fatalf("writes to an abandoned generation: %v", err)
	}
}

func TestScratchCheckpointOffsetsOverrideKafka(t *testing.T) {
	store := openScratch(t)
	topic := "test." + string(testID(t))
	batch := deliveries(eventing.DeliveryLive, paymentEvents(t, testID(t), 1)...)
	batch[0].Kafka = &KafkaPosition{Topic: topic, Partition: 2, Offset: 41, HighWatermark: 42}
	process(t, store, batch, nil)
	assigned, err := store.checkpointOffsets(context.Background(), PaymentConsumer, map[string]map[int32]kgoOffset{topic: {0: {}, 2: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(assigned[topic][2]); got != fmt.Sprint(atOffset(42)) {
		t.Fatalf("partition 2 resumes at %s", got)
	}
	if got := fmt.Sprint(assigned[topic][0]); got != fmt.Sprint(atStart()) {
		t.Fatalf("partition 0 without a checkpoint resumes at %s", got)
	}
}
