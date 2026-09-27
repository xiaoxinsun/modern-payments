package projections

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/twmb/franz-go/pkg/kgo"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

type kgoOffset = kgo.Offset

func atOffset(offset int64) kgo.Offset { return kgo.NewOffset().At(offset).WithEpoch(-1) }
func atStart() kgo.Offset              { return kgo.NewOffset().AtStart() }

func TestReplayedEventsNeverReachEffectfulConsumers(t *testing.T) {
	var notified, indexed []eventing.Delivery
	effects := []eventing.Consumer{
		{Name: "notifier", Effectful: true, Handle: func(context.Context, contracts.Event) error {
			notified = append(notified, "called")
			return nil
		}},
		{Name: "search-index", Handle: func(context.Context, contracts.Event) error {
			indexed = append(indexed, "called")
			return nil
		}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runEffects(context.Background(), logger, effects, []Applied{
		{Delivery: eventing.DeliveryLive}, {Delivery: eventing.DeliveryReplay}, {Delivery: eventing.DeliveryReplay},
	})
	if len(notified) != 1 || len(indexed) != 3 {
		t.Fatalf("effectful consumer saw %d events (want only the live one); non-effectful saw %d", len(notified), len(indexed))
	}
	if (eventing.Consumer{Effectful: true}).Accepts(eventing.DeliveryReplay) {
		t.Fatal("an effectful consumer must refuse replayed events")
	}
}

func TestDecodeRejectsMalformedEnvelopes(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":     `{"eventId":`,
		"unknown field": `{"eventId":"30000000-0000-4000-8000-000000000001","surprise":true}`,
		"trailing data": `{} {}`,
	} {
		if item := Decode([]byte(raw), eventing.DeliveryLive, nil); item.Event != nil || item.Problem == "" {
			t.Fatalf("%s: decoded %+v", name, item)
		}
	}
	valid := `{"eventId":"30000000-0000-4000-8000-000000000001","eventType":"PaymentReceived","schemaVersion":1,
"aggregateType":"Payment","aggregateId":"30000000-0000-4000-8000-000000000002","aggregateVersion":"1","eventSequence":"1",
"occurredAt":"2026-09-27T01:02:03Z","recordedAt":"2026-09-27T01:02:03Z","correlationId":"30000000-0000-4000-8000-000000000003",
"causationId":"30000000-0000-4000-8000-000000000003","source":"payment-api","payload":{}}`
	item := Decode([]byte(valid), eventing.DeliveryLive, nil)
	if item.Event == nil || checkEnvelope(*item.Event) != "" {
		t.Fatalf("valid envelope rejected: %+v", item)
	}
	item.Event.AggregateType = "CoreBankingPosting"
	if checkEnvelope(*item.Event) == "" {
		t.Fatal("a foreign aggregate type must not be projected as a payment")
	}
}

func TestCompareTimelineRequiresJournalPrefixUpToWatermark(t *testing.T) {
	ids := []uuid.UUID{uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4()), uuid.Must(uuid.NewV4())}
	journal := make([]JournalEntry, 3)
	rows := make([]projectiondb.GenerationTimelineRow, 3)
	for index := range 3 {
		journal[index] = JournalEntry{Position: int64(10 + index), Event: contracts.Event{
			EventID: contractsID(ids[index]), EventSequence: int64(index + 1), EventType: "T", AggregateVersion: int64(index + 1),
		}}
		rows[index] = projectiondb.GenerationTimelineRow{EventID: ids[index], EventSequence: int64(index + 1), EventType: "T", AggregateVersion: int64(index + 1)}
	}
	if problem := compareTimeline(journal, rows, 12); problem != "" {
		t.Fatal(problem)
	}
	if problem := compareTimeline(journal, rows[:2], 11); problem != "" {
		t.Fatalf("an event above the watermark may still be arriving: %s", problem)
	}
	if compareTimeline(journal, rows[:2], 12) == "" {
		t.Fatal("a missing event at the watermark must fail verification")
	}
	swapped := []projectiondb.GenerationTimelineRow{rows[1], rows[0], rows[2]}
	if compareTimeline(journal, swapped, 12) == "" {
		t.Fatal("a reordered timeline must fail verification")
	}
	if compareTimeline(journal[:2], rows, 11) == "" {
		t.Fatal("events absent from the journal must fail verification")
	}
}

func contractsID(id uuid.UUID) value.ID { return value.ID(id.String()) }
