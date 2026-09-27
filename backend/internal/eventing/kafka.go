// Package eventing moves committed domain events between owners: outbox relays
// publish journal rows to Kafka, and consumers read them back. franz-go supplies
// the Kafka protocol mechanics; publication, deduplication, ordering-gap and
// replay semantics stay in GTB code (ADR 0010).
package eventing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"gtb.local/backend/internal/contracts"
)

// Topics are created explicitly (make kafka-topics). Records are keyed by
// aggregate ID, so one aggregate's events share a partition.
const (
	PaymentEventsTopic       = "gtb.payment.events.v1"
	CoreBankingPostingsTopic = "gtb.corebanking.postings.v1"
)

// Brokers reads GTB_KAFKA_BROKERS (comma separated), defaulting to the local
// loopback listener.
func Brokers() []string {
	raw := os.Getenv("GTB_KAFKA_BROKERS")
	if raw == "" {
		return []string{"127.0.0.1:9092"}
	}
	var brokers []string
	for _, broker := range strings.Split(raw, ",") {
		if broker = strings.TrimSpace(broker); broker != "" {
			brokers = append(brokers, broker)
		}
	}
	return brokers
}

// NewProducer returns an idempotent producer that waits for all in-sync
// replicas. The client connects lazily, so an unavailable broker does not stop
// the owning service; undelivered events simply stay in its outbox.
//
// Delivery is time-bounded: by default franz-go refuses to fail an in-flight
// idempotent record, which would hold a relay's outbox transaction open for as
// long as the broker is down. Allowing cancellation means a relay retry may
// write an event twice; consumers deduplicate by event ID, which is the
// at-least-once contract anyway.
func NewProducer(brokers []string, clientID string) (*kgo.Client, error) {
	if len(brokers) == 0 {
		return nil, errors.New("at least one Kafka broker is required")
	}
	return kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.AllowIdempotentProduceCancellation(),
		kgo.RecordDeliveryTimeout(10*time.Second),
		kgo.ProduceRequestTimeout(10*time.Second),
	)
}

// Publisher delivers events to one topic and reports a per-event result in
// input order. A nil error means the broker acknowledged the record.
type Publisher interface {
	Publish(ctx context.Context, topic string, events []contracts.Event) []error
}

type KafkaPublisher struct {
	Client *kgo.Client
}

func (publisher KafkaPublisher) Publish(ctx context.Context, topic string, events []contracts.Event) []error {
	results := make([]error, len(events))
	records := make([]*kgo.Record, 0, len(events))
	index := make(map[*kgo.Record]int, len(events))
	for position, event := range events {
		record, err := EventRecord(topic, event)
		if err != nil {
			results[position] = err
			continue
		}
		index[record] = position
		records = append(records, record)
	}
	if len(records) == 0 {
		return results
	}
	// ProduceSync reports results in completion order; map them back by record.
	for _, result := range publisher.Client.ProduceSync(ctx, records...) {
		results[index[result.Record]] = result.Err
	}
	return results
}

// EventRecord encodes a validated envelope. Headers duplicate routing metadata
// so tooling can filter without parsing the body; the body stays authoritative.
func EventRecord(topic string, event contracts.Event) (*kgo.Record, error) {
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("invalid event %s: %w", event.EventID, err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return &kgo.Record{
		Topic: topic, Key: []byte(event.AggregateID), Value: body,
		Headers: []kgo.RecordHeader{
			{Key: "eventId", Value: []byte(event.EventID)},
			{Key: "eventType", Value: []byte(event.EventType)},
			{Key: "schemaVersion", Value: []byte(fmt.Sprint(event.SchemaVersion))},
		},
	}, nil
}
