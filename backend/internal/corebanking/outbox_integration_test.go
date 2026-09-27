package corebanking

import (
	"context"
	"errors"
	"os"
	"testing"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/corebanking/domain"
	"gtb.local/backend/internal/platform/value"
)

// Delivery marks outbox rows published, so this runs against a scratch Core
// Banking database (make eventing-integration).
func TestScratchPostingOutboxDelivery(t *testing.T) {
	databaseURL := os.Getenv("GTB_COREBANKING_SCRATCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GTB_COREBANKING_SCRATCH_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	service := NewService(pool)
	accept := func(_ context.Context, events []contracts.Event) []error { return make([]error, len(events)) }
	for {
		result, err := service.Deliver(ctx, 500, accept)
		if err != nil {
			t.Fatal(err)
		}
		if result.Published == 0 {
			break
		}
	}
	debtor, creditor := newTestID(t), newTestID(t)
	for _, account := range []value.ID{debtor, creditor} {
		if _, err := pool.Exec(ctx, `INSERT INTO app.accounts (account_id, external_reference, customer_id, account_kind, currency, booked_balance_minor)
			VALUES ($1, $2, $1, 'CUSTOMER_DEPOSIT', 'SGD', 5000)`, account, "test/"+string(account)); err != nil {
			t.Fatal(err)
		}
	}
	posting, err := service.Post(ctx, PostRequest{
		OperationID: newTestID(t), Description: "outbox test", CorrelationID: newTestID(t),
		Entries: []domain.Entry{
			{AccountID: debtor, Side: domain.Debit, Amount: value.Money{AmountMinor: 700, Currency: value.SGD}},
			{AccountID: creditor, Side: domain.Credit, Amount: value.Money{AmountMinor: 700, Currency: value.SGD}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.Deliver(ctx, 10, func(_ context.Context, events []contracts.Event) []error {
		return []error{errors.New("broker unavailable")}
	})
	if err != nil || failed.Failed != 1 || failed.Published != 0 {
		t.Fatalf("failed delivery: %+v %v", failed, err)
	}
	var delivered contracts.Event
	result, err := service.Deliver(ctx, 10, func(_ context.Context, events []contracts.Event) []error {
		if len(events) != 1 {
			t.Fatalf("expected the one posting event, got %d", len(events))
		}
		delivered = events[0]
		return []error{nil}
	})
	if err != nil || result.Published != 1 {
		t.Fatalf("delivery: %+v %v", result, err)
	}
	if err := delivered.Validate(); err != nil || delivered.EventType != "CoreBankingPostingCommitted" || delivered.AggregateID != posting.PostingID {
		t.Fatalf("delivered %+v: %v", delivered, err)
	}
	var attempts int
	var lastError *string
	if err := pool.QueryRow(ctx, `SELECT delivery_attempts, last_error FROM app.posting_outbox WHERE event_id = $1 AND published_at IS NOT NULL`,
		string(delivered.EventID)).Scan(&attempts, &lastError); err != nil || attempts != 2 || lastError != nil {
		t.Fatalf("outbox row: attempts %d error %v: %v", attempts, lastError, err)
	}
}
