package corebanking

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"gtb.local/backend/internal/corebanking/domain"
	"gtb.local/backend/internal/platform/value"
)

type testLedger struct {
	service     *Service
	maya        value.ID
	daniel      value.ID
	clearing    value.ID
	settlement  value.ID
	suspense    value.ID
	correlation value.ID
}

func postgresService(t *testing.T) *Service {
	t.Helper()
	databaseURL := os.Getenv("GTB_COREBANKING_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GTB_COREBANKING_TEST_DATABASE_URL is not set")
	}
	pool, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewService(pool)
}

func newTestLedger(t *testing.T, mayaBalance, danielBalance, settlementBalance int64) testLedger {
	t.Helper()
	service := postgresService(t)
	ids := make([]value.ID, 6)
	for index := range ids {
		ids[index] = newTestID(t)
	}
	accounts := []struct {
		id       value.ID
		kind     domain.AccountKind
		balance  int64
		customer *value.ID
	}{
		{ids[0], domain.CustomerDeposit, mayaBalance, &ids[0]},
		{ids[1], domain.CustomerDeposit, danielBalance, &ids[1]},
		{ids[2], domain.OutgoingClearing, 0, nil},
		{ids[3], domain.SettlementNostro, settlementBalance, nil},
		{ids[4], domain.IncomingSuspense, 0, nil},
	}
	for _, account := range accounts {
		_, err := service.pool.Exec(context.Background(), `
INSERT INTO app.accounts (
  account_id, external_reference, customer_id, account_kind, currency, booked_balance_minor
) VALUES ($1, $2, $3, $4, 'SGD', $5)`,
			account.id, "test/"+string(account.id), account.customer, account.kind, account.balance)
		if err != nil {
			t.Fatal(err)
		}
	}
	return testLedger{
		service: service, maya: ids[0], daniel: ids[1], clearing: ids[2],
		settlement: ids[3], suspense: ids[4], correlation: ids[5],
	}
}

func newTestID(t *testing.T) value.ID {
	t.Helper()
	id, err := value.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (ledger testLedger) reserve(t *testing.T, account value.ID, amount int64) domain.Hold {
	t.Helper()
	hold, err := ledger.service.Reserve(context.Background(), ReserveRequest{
		OperationID: newTestID(t), AccountID: account,
		Amount:        value.Money{AmountMinor: amount, Currency: value.SGD},
		CorrelationID: ledger.correlation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return hold
}

func (ledger testLedger) post(t *testing.T, hold *value.ID, description string, entries ...domain.Entry) domain.Posting {
	t.Helper()
	posting, err := ledger.service.Post(context.Background(), PostRequest{
		OperationID: newTestID(t), HoldID: hold, Description: description,
		Entries: entries, CorrelationID: ledger.correlation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return posting
}

func line(account value.ID, side domain.Side, amount int64) domain.Entry {
	return domain.Entry{AccountID: account, Side: side, Amount: value.Money{AmountMinor: amount, Currency: value.SGD}}
}

func balance(t *testing.T, ledger testLedger, account value.ID, booked, available int64) {
	t.Helper()
	got, err := ledger.service.GetAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if got.BookedMinor != booked || got.AvailableMinor != available {
		t.Fatalf("account %s: got booked/available %d/%d, want %d/%d", account, got.BookedMinor, got.AvailableMinor, booked, available)
	}
}

func TestPostgresAccountingScenariosAndImmutability(t *testing.T) {
	ctx := context.Background()

	t.Run("A successful outbound and C return", func(t *testing.T) {
		ledger := newTestLedger(t, 100000, 20000, 500000)
		hold := ledger.reserve(t, ledger.maya, 10000)
		balance(t, ledger, ledger.maya, 100000, 90000)
		customerDebit := ledger.post(t, &hold.HoldID, "A2 customer debit",
			line(ledger.maya, domain.Debit, 10000), line(ledger.clearing, domain.Credit, 10000))
		balance(t, ledger, ledger.maya, 90000, 90000)
		balance(t, ledger, ledger.clearing, 10000, 10000)
		ledger.post(t, nil, "A4 external settlement",
			line(ledger.clearing, domain.Debit, 10000), line(ledger.settlement, domain.Credit, 10000))
		balance(t, ledger, ledger.clearing, 0, 0)
		balance(t, ledger, ledger.settlement, 490000, 490000)
		ledger.post(t, nil, "C1 evidenced return",
			line(ledger.settlement, domain.Debit, 10000), line(ledger.maya, domain.Credit, 10000))
		balance(t, ledger, ledger.maya, 100000, 100000)

		if _, err := ledger.service.pool.Exec(ctx, `UPDATE app.posting_entries SET amount_minor = 1 WHERE posting_id = $1`, customerDebit.PostingID); err == nil {
			t.Fatal("immutable posting entry accepted an update")
		}
		var events, outbox int
		if err := ledger.service.pool.QueryRow(ctx, `SELECT count(*) FROM app.posting_events WHERE aggregate_id = $1`, customerDebit.PostingID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := ledger.service.pool.QueryRow(ctx, `SELECT count(*) FROM app.posting_outbox WHERE event_id = $1`, customerDebit.EventID).Scan(&outbox); err != nil {
			t.Fatal(err)
		}
		if events != 1 || outbox != 1 {
			t.Fatalf("posting event/outbox = %d/%d, want 1/1", events, outbox)
		}

		unbalancedOperation := newTestID(t)
		unbalancedPosting := newTestID(t)
		transaction, err := ledger.service.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `
INSERT INTO app.operations (operation_id, operation_type, request_hash, resource_type, resource_id, correlation_id)
VALUES ($1, 'POST', $2, 'POSTING', $3, $4)`, unbalancedOperation,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			unbalancedPosting, ledger.correlation); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `
INSERT INTO app.posting_journals (posting_id, operation_id, description)
VALUES ($1, $2, 'unbalanced direct write must fail')`, unbalancedPosting, unbalancedOperation); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `
INSERT INTO app.posting_entries (posting_id, line_number, account_id, side, amount_minor, currency)
VALUES ($1, 1, $2, 'DEBIT', 1, 'SGD')`, unbalancedPosting, ledger.settlement); err != nil {
			t.Fatal(err)
		}
		if err := transaction.Commit(ctx); err == nil {
			t.Fatal("database accepted an unbalanced direct journal write")
		}
	})

	t.Run("B rejection branches and reversal", func(t *testing.T) {
		ledger := newTestLedger(t, 100000, 0, 500000)
		hold := ledger.reserve(t, ledger.maya, 10000)
		releaseOperation := newTestID(t)
		released, err := ledger.service.Release(ctx, ReleaseRequest{
			OperationID: releaseOperation, HoldID: hold.HoldID, CorrelationID: ledger.correlation,
		})
		if err != nil || released.Status != domain.HoldReleased {
			t.Fatalf("release: %v, %v", released.Status, err)
		}
		balance(t, ledger, ledger.maya, 100000, 100000)

		hold = ledger.reserve(t, ledger.maya, 10000)
		original := ledger.post(t, &hold.HoldID, "A2 before definite rejection",
			line(ledger.maya, domain.Debit, 10000), line(ledger.clearing, domain.Credit, 10000))
		reversal, err := ledger.service.Reverse(ctx, ReverseRequest{
			OperationID: newTestID(t), OriginalPostingID: original.PostingID,
			Description: "B compensating reversal", CorrelationID: ledger.correlation,
		})
		if err != nil {
			t.Fatal(err)
		}
		if reversal.ReversalOfPostingID == nil || *reversal.ReversalOfPostingID != original.PostingID {
			t.Fatal("reversal did not retain original posting reference")
		}
		balance(t, ledger, ledger.maya, 100000, 100000)
		balance(t, ledger, ledger.clearing, 0, 0)
		statement, err := ledger.service.Statement(ctx, ledger.maya, nil, nil, 100)
		if err != nil || len(statement) != 2 {
			t.Fatalf("statement entries = %d, %v", len(statement), err)
		}
	})

	t.Run("D book transfer", func(t *testing.T) {
		ledger := newTestLedger(t, 100000, 20000, 500000)
		hold := ledger.reserve(t, ledger.maya, 10000)
		ledger.post(t, &hold.HoldID, "D book transfer",
			line(ledger.maya, domain.Debit, 10000), line(ledger.daniel, domain.Credit, 10000))
		balance(t, ledger, ledger.maya, 90000, 90000)
		balance(t, ledger, ledger.daniel, 30000, 30000)
	})

	t.Run("E direct and suspense inbound", func(t *testing.T) {
		ledger := newTestLedger(t, 0, 20000, 500000)
		ledger.post(t, nil, "E1 direct inbound",
			line(ledger.settlement, domain.Debit, 10000), line(ledger.daniel, domain.Credit, 10000))
		balance(t, ledger, ledger.daniel, 30000, 30000)
		ledger.post(t, nil, "E2a inbound suspense",
			line(ledger.settlement, domain.Debit, 10000), line(ledger.suspense, domain.Credit, 10000))
		balance(t, ledger, ledger.suspense, 10000, 10000)
		ledger.post(t, nil, "E2b suspense clearance",
			line(ledger.suspense, domain.Debit, 10000), line(ledger.daniel, domain.Credit, 10000))
		balance(t, ledger, ledger.suspense, 0, 0)
		balance(t, ledger, ledger.daniel, 40000, 40000)
	})
}

func TestPostgresConcurrencyLostReplyAndConflict(t *testing.T) {
	ctx := context.Background()
	ledger := newTestLedger(t, 10000, 0, 500000)
	requests := []ReserveRequest{
		{OperationID: newTestID(t), AccountID: ledger.maya, Amount: value.Money{AmountMinor: 7000, Currency: value.SGD}, CorrelationID: ledger.correlation},
		{OperationID: newTestID(t), AccountID: ledger.maya, Amount: value.Money{AmountMinor: 7000, Currency: value.SGD}, CorrelationID: ledger.correlation},
	}
	results := make(chan error, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Add(1)
		go func(command ReserveRequest) {
			defer wait.Done()
			_, err := ledger.service.Reserve(ctx, command)
			results <- err
		}(request)
	}
	wait.Wait()
	close(results)
	var successes, insufficient int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInsufficientFunds):
			insufficient++
		default:
			t.Fatalf("unexpected concurrent reserve error: %v", err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("concurrent reserve success/insufficient = %d/%d", successes, insufficient)
	}
	balance(t, ledger, ledger.maya, 10000, 3000)

	duplicateLedger := newTestLedger(t, 100000, 0, 500000)
	reserve := ReserveRequest{
		OperationID: newTestID(t), AccountID: duplicateLedger.maya,
		Amount:        value.Money{AmountMinor: 10000, Currency: value.SGD},
		CorrelationID: duplicateLedger.correlation,
	}
	firstHold, err := duplicateLedger.service.Reserve(ctx, reserve)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a lost reply: the caller knows only its durable operation ID.
	secondHold, err := duplicateLedger.service.Reserve(ctx, reserve)
	if err != nil || secondHold.HoldID != firstHold.HoldID {
		t.Fatalf("lost-reply reserve returned %s instead of %s: %v", secondHold.HoldID, firstHold.HoldID, err)
	}
	conflictingReserve := reserve
	conflictingReserve.Amount.AmountMinor++
	if _, err := duplicateLedger.service.Reserve(ctx, conflictingReserve); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting reserve reuse: %v", err)
	}

	post := PostRequest{
		OperationID: newTestID(t), HoldID: &firstHold.HoldID, Description: "idempotent posting",
		Entries: []domain.Entry{
			line(duplicateLedger.maya, domain.Debit, 10000),
			line(duplicateLedger.clearing, domain.Credit, 10000),
		}, CorrelationID: duplicateLedger.correlation,
	}
	firstPosting, err := duplicateLedger.service.Post(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	secondPosting, err := duplicateLedger.service.Post(ctx, post)
	if err != nil || secondPosting.PostingID != firstPosting.PostingID || secondPosting.EventID != firstPosting.EventID {
		t.Fatalf("lost-reply post returned a different effect: %+v / %v", secondPosting, err)
	}
	post.Description = "conflicting posting"
	if _, err := duplicateLedger.service.Post(ctx, post); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting post reuse: %v", err)
	}
	operation, err := duplicateLedger.service.GetOperation(ctx, post.OperationID)
	if err != nil || operation.ResourceID != firstPosting.PostingID {
		t.Fatalf("operation lookup = %+v, %v", operation, err)
	}
	var postings, entries, events int
	query := `SELECT
  (SELECT count(*) FROM app.posting_journals WHERE operation_id = $1),
  (SELECT count(*) FROM app.posting_entries WHERE posting_id = $2),
  (SELECT count(*) FROM app.posting_events WHERE aggregate_id = $2)`
	if err := duplicateLedger.service.pool.QueryRow(ctx, query, post.OperationID, firstPosting.PostingID).Scan(&postings, &entries, &events); err != nil {
		t.Fatal(err)
	}
	if postings != 1 || entries != 2 || events != 1 {
		t.Fatalf("duplicate effect counts postings/entries/events = %d/%d/%d", postings, entries, events)
	}
	fmt.Printf("verified durable operation %s -> posting %s\n", post.OperationID, firstPosting.PostingID)
}
