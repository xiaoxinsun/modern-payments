package eventing

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"gtb.local/backend/internal/contracts"
)

// OutboxSource is implemented by each owner's storage. Deliver locks up to limit
// undelivered outbox rows (skipping rows another relay holds), calls publish
// with their envelopes in journal order, and records each result in the same
// database transaction. A row is marked published only after publish reports
// broker acknowledgement, so a crash between the two republishes the same
// event ID (at least once).
type OutboxSource interface {
	Deliver(ctx context.Context, limit int, publish func(context.Context, []contracts.Event) []error) (DeliveryResult, error)
}

type DeliveryResult struct {
	Published int
	Failed    int
}

type Relay struct {
	Name      string
	Topic     string
	Source    OutboxSource
	Publisher Publisher
	Logger    *slog.Logger
	BatchSize int
	// Idle is the poll interval once the outbox is drained.
	Idle time.Duration
	// MaxBackoff bounds the wait after a failed delivery attempt.
	MaxBackoff time.Duration
}

var relayMeter = otel.Meter("gtb.local/backend/internal/eventing")

// Run delivers until ctx is cancelled. Broker or database failures back off
// and retry; events stay in the outbox until acknowledged.
func (relay *Relay) Run(ctx context.Context) {
	published, _ := relayMeter.Int64Counter("gtb.outbox.published", metric.WithDescription("Outbox events acknowledged by Kafka"))
	failed, _ := relayMeter.Int64Counter("gtb.outbox.failed", metric.WithDescription("Outbox delivery attempts that failed"))
	attributes := metric.WithAttributes(attribute.String("relay", relay.Name))
	backoff := relay.Idle
	relay.Logger.Info("outbox relay started", "relay", relay.Name, "topic", relay.Topic)
	for {
		result, err := relay.RunOnce(ctx)
		if ctx.Err() != nil {
			relay.Logger.Info("outbox relay stopped", "relay", relay.Name)
			return
		}
		published.Add(ctx, int64(result.Published), attributes)
		failed.Add(ctx, int64(result.Failed), attributes)
		wait := relay.Idle
		switch {
		case err != nil || result.Failed > 0:
			if err != nil {
				relay.Logger.Warn("outbox delivery failed", "relay", relay.Name, "error", err)
			} else {
				relay.Logger.Warn("outbox events not acknowledged; retrying", "relay", relay.Name, "failed", result.Failed, "published", result.Published)
			}
			backoff = min(max(backoff*2, relay.Idle), relay.MaxBackoff)
			wait = backoff
		case result.Published > 0:
			backoff = relay.Idle
			relay.Logger.Info("outbox events published", "relay", relay.Name, "published", result.Published)
			if result.Published == relay.BatchSize {
				wait = 0
			}
		default:
			backoff = relay.Idle
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				relay.Logger.Info("outbox relay stopped", "relay", relay.Name)
				return
			case <-time.After(wait):
			}
		}
	}
}

// RunOnce delivers at most one batch.
func (relay *Relay) RunOnce(ctx context.Context) (DeliveryResult, error) {
	return relay.Source.Deliver(ctx, relay.BatchSize, func(ctx context.Context, events []contracts.Event) []error {
		produceContext, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return relay.Publisher.Publish(produceContext, relay.Topic, events)
	})
}

// NewRelay applies the local defaults.
func NewRelay(name, topic string, source OutboxSource, publisher Publisher, logger *slog.Logger) *Relay {
	return &Relay{
		Name: name, Topic: topic, Source: source, Publisher: publisher, Logger: logger,
		BatchSize: 100, Idle: 250 * time.Millisecond, MaxBackoff: 10 * time.Second,
	}
}

// DeliveryError keeps stored outbox errors bounded and free of payload data.
func DeliveryError(err error) string {
	message := strings.ToValidUTF8(err.Error(), "")
	if len(message) > 500 {
		message = strings.ToValidUTF8(message[:500], "")
	}
	if message == "" {
		message = "delivery failed"
	}
	return message
}
