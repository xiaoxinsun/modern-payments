package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/value"
)

// Outbox delivery marks rows published, so these tests run against a scratch
// payment database (make eventing-integration) instead of the shared one.
func openScratchStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("GTB_PAYMENT_SCRATCH_DATABASE_URL")
	if url == "" {
		t.Skip("GTB_PAYMENT_SCRATCH_DATABASE_URL is not set")
	}
	pool, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

func drain(t *testing.T, store *Store) {
	t.Helper()
	for {
		result, err := store.Deliver(context.Background(), 500, func(_ context.Context, events []contracts.Event) []error {
			return make([]error, len(events))
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Published == 0 {
			return
		}
	}
}

func TestScratchOutboxMarksOnlyAcknowledgedEvents(t *testing.T) {
	store := openScratchStore(t)
	ctx := context.Background()
	drain(t, store)
	customer := newID(t)
	var paymentIDs []value.ID
	for index, key := range []string{"outbox-1", "outbox-2", "outbox-3"} {
		receipt, err := store.Receive(ctx, newReceipt(t, customer, key, int64(100+index)))
		if err != nil {
			t.Fatal(err)
		}
		paymentIDs = append(paymentIDs, receipt.Payment.ID)
	}
	var published []contracts.Event
	result, err := store.Deliver(ctx, 10, func(_ context.Context, events []contracts.Event) []error {
		results := make([]error, len(events))
		for index, event := range events {
			if err := event.Validate(); err != nil {
				t.Fatalf("relay produced an invalid envelope: %v", err)
			}
			if event.AggregateID == paymentIDs[1] {
				results[index] = errors.New("broker unavailable")
				continue
			}
			published = append(published, event)
		}
		return results
	})
	if err != nil || result.Published != 2 || result.Failed != 1 {
		t.Fatalf("first delivery: %+v %v", result, err)
	}
	var attempts int
	var lastError *string
	if err := store.pool.QueryRow(ctx, `SELECT o.delivery_attempts, o.last_error FROM app.outbox o JOIN app.domain_events e USING (event_id)
		WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, string(paymentIDs[1])).Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError == nil || *lastError != "broker unavailable" {
		t.Fatalf("failed row: attempts %d error %v", attempts, lastError)
	}
	// Retry publishes the same event identity, and only that one.
	retried, err := store.Deliver(ctx, 10, func(_ context.Context, events []contracts.Event) []error {
		if len(events) != 1 || events[0].AggregateID != paymentIDs[1] || events[0].EventType != PaymentReceivedEvent {
			t.Fatalf("retry offered %+v", events)
		}
		return []error{nil}
	})
	if err != nil || retried.Published != 1 {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	var cleared *string
	if err := store.pool.QueryRow(ctx, `SELECT o.last_error FROM app.outbox o JOIN app.domain_events e USING (event_id)
		WHERE e.aggregate_id = $1`, string(paymentIDs[1])).Scan(&cleared); err != nil || cleared != nil {
		t.Fatalf("published row keeps an error: %v %v", cleared, err)
	}
	empty, err := store.Deliver(ctx, 10, func(context.Context, []contracts.Event) []error {
		t.Fatal("nothing should remain to publish")
		return nil
	})
	if err != nil || empty.Published != 0 {
		t.Fatalf("drained outbox: %+v %v", empty, err)
	}
	// A publisher that never returns (crash before acknowledgement) leaves the
	// row undelivered: the transaction rolls back.
	receipt, err := store.Receive(ctx, newReceipt(t, customer, "outbox-crash", 900))
	if err != nil {
		t.Fatal(err)
	}
	crashContext, cancel := context.WithCancel(ctx)
	if _, err := store.Deliver(crashContext, 10, func(context.Context, []contracts.Event) []error {
		cancel()
		return []error{nil}
	}); err == nil {
		t.Fatal("delivery committed after its context was cancelled")
	}
	var pending int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM app.outbox o JOIN app.domain_events e USING (event_id)
		WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, string(receipt.Payment.ID)).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("crashed delivery lost the event: %d %v", pending, err)
	}
	drain(t, store)
}

func TestScratchConcurrentRelaysNeverShareRows(t *testing.T) {
	store := openScratchStore(t)
	ctx := context.Background()
	drain(t, store)
	customer := newID(t)
	for index := range 6 {
		if _, err := store.Receive(ctx, newReceipt(t, customer, "concurrent-"+string(rune('a'+index)), int64(200+index))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[value.ID]int{}
	var wait sync.WaitGroup
	for range 3 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Deliver(ctx, 4, func(_ context.Context, events []contracts.Event) []error {
				time.Sleep(200 * time.Millisecond) // hold row locks while "Kafka" acknowledges
				mu.Lock()
				defer mu.Unlock()
				for _, event := range events {
					seen[event.EventID]++
				}
				return make([]error, len(events))
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("event %s published by %d relays at once", id, count)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("relays published %d of 6 events", len(seen))
	}
}

func TestScratchJournalPagesAndStampsResultingState(t *testing.T) {
	store := openScratchStore(t)
	ctx := context.Background()
	receipt, err := store.Receive(ctx, newReceipt(t, newID(t), "journal-1", 300))
	if err != nil {
		t.Fatal(err)
	}
	id := receipt.Payment.ID
	if _, err := store.Apply(ctx, ApplyRequest{
		PaymentID: id, Action: "VALIDATE", CorrelationID: newID(t), Mutate: transition(domain.Validating),
		Events: []NewEvent{{Type: "PaymentValidationStarted", Payload: map[string]any{"lifecycleState": "FORGED"}}, {Type: "PaymentNoted", Payload: map[string]any{"note": "x"}}},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.AggregateJournal(ctx, id, 0, 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("aggregate journal: %d %v", len(entries), err)
	}
	for index, entry := range entries {
		if entry.Event.EventSequence != int64(index+1) || entry.Event.Validate() != nil {
			t.Fatalf("entry %d: %+v", index, entry.Event)
		}
		if index > 0 && entry.Position <= entries[index-1].Position {
			t.Fatal("journal positions must increase within an aggregate")
		}
		var payload map[string]any
		if err := json.Unmarshal(entry.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{true: "RECEIVED", false: "VALIDATING"}[index == 0]
		if payload["paymentId"] != string(id) || payload["lifecycleState"] != want || payload["executionOutcome"] != "NOT_SUBMITTED" {
			t.Fatalf("entry %d payload does not carry the resulting state: %v", index, payload)
		}
	}
	after, err := store.AggregateJournal(ctx, id, 2, 10)
	if err != nil || len(after) != 1 || after[0].Event.EventType != "PaymentNoted" {
		t.Fatalf("after sequence 2: %+v %v", after, err)
	}
	// A payload journaled before B03 stamping is upcast from version history.
	legacyID := newID(t)
	if _, err := store.pool.Exec(ctx, `INSERT INTO app.domain_events (event_id, event_type, schema_version, aggregate_type, aggregate_id,
		aggregate_version, event_sequence, occurred_at, correlation_id, causation_id, source, payload)
		VALUES ($1, 'PaymentNoted', 1, 'Payment', $2, 2, 99, now(), $1, $1, 'payment-api', '{"note":"legacy"}')`, string(legacyID), string(id)); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.AggregateJournal(ctx, id, 98, 10)
	if err != nil || len(legacy) != 1 {
		t.Fatalf("legacy event: %+v %v", legacy, err)
	}
	var upcast map[string]any
	if err := json.Unmarshal(legacy[0].Event.Payload, &upcast); err != nil || upcast["note"] != "legacy" ||
		upcast["lifecycleState"] != "VALIDATING" || upcast["executionOutcome"] != "NOT_SUBMITTED" || upcast["paymentId"] != string(id) {
		t.Fatalf("upcast payload: %v %v", upcast, err)
	}
	var stored string
	if err := store.pool.QueryRow(ctx, `SELECT payload::text FROM app.domain_events WHERE event_id = $1`, string(legacyID)).Scan(&stored); err != nil || stored != `{"note": "legacy"}` {
		t.Fatalf("the journal itself must not be rewritten: %s %v", stored, err)
	}

	watermark, err := store.JournalWatermark(ctx)
	if err != nil || watermark.Position < entries[2].Position || watermark.Events < 3 || watermark.Aggregates < 1 {
		t.Fatalf("watermark: %+v %v", watermark, err)
	}
	page, err := store.Journal(ctx, entries[0].Position-1, entries[1].Position, 10)
	if err != nil || len(page) != 2 || page[0].Event.EventID != entries[0].Event.EventID || page[1].Position != entries[1].Position {
		t.Fatalf("bounded page: %+v %v", page, err)
	}
	limited, err := store.Journal(ctx, 0, watermark.Position, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limited page: %d %v", len(limited), err)
	}
	drain(t, store)
}
