package eventing

import (
	"context"

	"gtb.local/backend/internal/contracts"
)

// Delivery says how an event reached a consumer. LIVE events come from Kafka.
// REPLAY events are re-read from an owner's journal to rebuild a projection or
// fill an ordering gap; they describe facts that already happened.
type Delivery string

const (
	DeliveryLive   Delivery = "LIVE"
	DeliveryReplay Delivery = "REPLAY"
)

// Consumer is a handler of committed events. An effectful consumer acts outside
// its own database (notifications, commands, external calls).
type Consumer struct {
	Name      string
	Effectful bool
	Handle    func(context.Context, contracts.Event) error
}

// Accepts enforces replay suppression: replayed events never reach effectful
// consumers, so rebuilding projections cannot move money or notify anyone
// again.
func (consumer Consumer) Accepts(delivery Delivery) bool {
	return delivery == DeliveryLive || !consumer.Effectful
}
