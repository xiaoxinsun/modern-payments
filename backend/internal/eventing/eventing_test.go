package eventing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gtb.local/backend/internal/contracts"
)

func validEvent() contracts.Event {
	at := time.Date(2026, 9, 27, 1, 2, 3, 0, time.UTC)
	return contracts.Event{
		EventID: "30000000-0000-4000-8000-000000000001", EventType: "PaymentReceived", SchemaVersion: 1,
		AggregateType: "Payment", AggregateID: "30000000-0000-4000-8000-000000000002", AggregateVersion: 1, EventSequence: 1,
		OccurredAt: at, RecordedAt: at, CorrelationID: "30000000-0000-4000-8000-000000000003",
		CausationID: "30000000-0000-4000-8000-000000000003", Source: "payment-api", Payload: json.RawMessage(`{}`),
	}
}

func TestEventRecordIsKeyedByAggregateAndRejectsInvalidEnvelopes(t *testing.T) {
	event := validEvent()
	record, err := EventRecord(PaymentEventsTopic, event)
	if err != nil {
		t.Fatal(err)
	}
	if string(record.Key) != string(event.AggregateID) || record.Topic != PaymentEventsTopic {
		t.Fatalf("record routing: key %q topic %q", record.Key, record.Topic)
	}
	var decoded contracts.Event
	if err := json.Unmarshal(record.Value, &decoded); err != nil || decoded.EventID != event.EventID || decoded.EventSequence != 1 {
		t.Fatalf("record body: %+v %v", decoded, err)
	}
	headers := map[string]string{}
	for _, header := range record.Headers {
		headers[header.Key] = string(header.Value)
	}
	if headers["eventId"] != string(event.EventID) || headers["eventType"] != "PaymentReceived" || headers["schemaVersion"] != "1" {
		t.Fatalf("headers: %v", headers)
	}
	event.EventSequence = 0
	if _, err := EventRecord(PaymentEventsTopic, event); err == nil {
		t.Fatal("an invalid envelope must never be published")
	}
}

type scriptedSource struct {
	results []DeliveryResult
	calls   atomic.Int32
}

func (source *scriptedSource) Deliver(ctx context.Context, _ int, publish func(context.Context, []contracts.Event) []error) (DeliveryResult, error) {
	call := int(source.calls.Add(1))
	if call > len(source.results) {
		<-ctx.Done()
		return DeliveryResult{}, ctx.Err()
	}
	if errs := publish(ctx, []contracts.Event{validEvent()}); len(errs) != 1 {
		return DeliveryResult{}, errors.New("publisher result count mismatch")
	}
	return source.results[call-1], nil
}

type deadlinePublisher struct{ sawDeadline atomic.Bool }

func (publisher *deadlinePublisher) Publish(ctx context.Context, _ string, events []contracts.Event) []error {
	_, bounded := ctx.Deadline()
	publisher.sawDeadline.Store(bounded)
	return make([]error, len(events))
}

func TestRelayRetriesFailuresAndStopsWithContext(t *testing.T) {
	source := &scriptedSource{results: []DeliveryResult{{Failed: 1}, {Failed: 1}, {Published: 1}}}
	publisher := &deadlinePublisher{}
	relay := NewRelay("test", PaymentEventsTopic, source, publisher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay.Idle, relay.MaxBackoff = time.Millisecond, 4*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { relay.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for source.calls.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("relay stopped retrying after %d calls", source.calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if !publisher.sawDeadline.Load() {
		t.Fatal("each Kafka acknowledgement wait must be bounded")
	}
}

func TestDeliveryErrorIsBoundedAndNeverEmpty(t *testing.T) {
	if got := DeliveryError(errors.New(strings.Repeat("é", 400))); len(got) > 500 || !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Fatalf("bounded error: %d bytes", len(got))
	}
	if got := DeliveryError(errors.New("")); got == "" {
		t.Fatal("stored delivery errors must not be empty")
	}
}
