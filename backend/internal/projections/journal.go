package projections

import (
	"context"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/platform/value"
)

// JournalEntry is one committed payment event with its journal position.
type JournalEntry struct {
	Position int64
	Event    contracts.Event
}

// JournalWatermark is the payment journal's highest allocated position and the
// event and payment counts at or below it.
type JournalWatermark struct {
	Position int64 `json:"position"`
	Events   int64 `json:"events"`
	Payments int64 `json:"payments"`
}

// Journal reads the payment owner's canonical event journal through its API;
// the projector never reads payment tables.
type Journal interface {
	Watermark(ctx context.Context) (JournalWatermark, error)
	Page(ctx context.Context, afterPosition, upToPosition int64, limit int) ([]JournalEntry, error)
	PaymentEvents(ctx context.Context, paymentID value.ID, afterSequence int64, limit int) ([]JournalEntry, error)
}
