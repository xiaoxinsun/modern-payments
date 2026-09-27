package query

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/auth"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections"
)

// The service reads through the read-only query_reader login; test data is
// written through the projector with the owner login.
func openScratch(t *testing.T) (*projections.Store, *Service, *fakeReceipts) {
	t.Helper()
	ownerURL, readerURL := os.Getenv("GTB_PROJECTIONS_SCRATCH_DATABASE_URL"), os.Getenv("GTB_PROJECTIONS_SCRATCH_READER_URL")
	if ownerURL == "" || readerURL == "" {
		t.Skip("GTB_PROJECTIONS_SCRATCH_DATABASE_URL and GTB_PROJECTIONS_SCRATCH_READER_URL are not set")
	}
	owner, err := projections.Open(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	reader, err := projections.Open(context.Background(), readerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	receipts := &fakeReceipts{}
	return projections.NewStore(owner), NewService(reader, receipts, 10*time.Second), receipts
}

type fakeReceipts struct {
	receipt       Receipt
	err           error
	authorization string
}

func (receipts *fakeReceipts) Receipt(_ context.Context, authorization string, paymentID value.ID) (Receipt, error) {
	receipts.authorization = authorization
	if receipts.err != nil {
		return Receipt{}, receipts.err
	}
	receipt := receipts.receipt
	receipt.PaymentID = paymentID
	return receipt, nil
}

func id(t *testing.T) value.ID {
	t.Helper()
	generated, err := value.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return generated
}

func received(t *testing.T, store *projections.Store, customerID value.ID, events int) value.ID {
	t.Helper()
	paymentID := id(t)
	states := []string{"RECEIVED", "VALIDATING", "ACCEPTED"}
	var items []projections.Incoming
	for index := range events {
		payload := map[string]any{"paymentId": paymentID, "lifecycleState": states[index], "executionOutcome": "NOT_SUBMITTED"}
		eventType := "PaymentValidationStarted"
		if index == 0 {
			eventType = "PaymentReceived"
			payload["customerId"], payload["product"] = customerID, "DOMESTIC_INSTANT"
			payload["amount"] = map[string]string{"amountMinor": "1234", "currency": "SGD"}
		}
		encoded, _ := json.Marshal(payload)
		at := time.Now().UTC().Truncate(time.Microsecond)
		event := contracts.Event{
			EventID: id(t), EventType: eventType, SchemaVersion: 1, AggregateType: "Payment", AggregateID: paymentID,
			AggregateVersion: int64(index + 1), EventSequence: int64(index + 1), OccurredAt: at, RecordedAt: at,
			CorrelationID: paymentID, CausationID: paymentID, Source: "payment-api", Payload: encoded,
		}
		items = append(items, projections.Incoming{Event: &event, Delivery: eventing.DeliveryLive})
	}
	if _, err := store.Process(context.Background(), projections.PaymentConsumer, items, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct receivedAt for stable ordering
	return paymentID
}

func customer(customerIDs ...value.ID) auth.Principal {
	scope := map[string]struct{}{}
	for _, customerID := range customerIDs {
		scope[string(customerID)] = struct{}{}
	}
	return auth.Principal{Subject: "customer-subject", Roles: map[string]struct{}{"customer": {}}, CustomerIDs: scope}
}

var operator = auth.Principal{Subject: "ops-subject", Roles: map[string]struct{}{"ops_viewer": {}}}

func TestScratchQueryScopesPagesAndReportsPending(t *testing.T) {
	store, service, receipts := openScratch(t)
	ctx := context.Background()
	mine, theirs := id(t), id(t)
	first := received(t, store, mine, 2)
	second := received(t, store, mine, 1)
	third := received(t, store, mine, 1)
	foreign := received(t, store, theirs, 1)

	view, err := service.Payment(ctx, customer(mine), "Bearer customer", first, 0)
	if err != nil || view.Status != Projected || view.Version != 2 || view.LifecycleState != "VALIDATING" || view.Amount.AmountMinor != 1234 {
		t.Fatalf("own payment: %+v %v", view, err)
	}
	if view, err := service.Payment(ctx, customer(mine), "Bearer customer", first, 3); err != nil || view.Status != Behind {
		t.Fatalf("minVersion above the projection: %+v %v", view, err)
	}
	if _, err := service.Payment(ctx, customer(mine), "Bearer customer", foreign, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign payment: %v", err)
	}
	if view, err := service.Payment(ctx, operator, "Bearer ops", foreign, 0); err != nil || view.CustomerID != theirs {
		t.Fatalf("operator read: %+v %v", view, err)
	}

	// Not yet projected: the command store's receipt answers, with the caller's token.
	receipts.receipt = Receipt{CustomerID: mine, Product: "BOOK_TRANSFER", Amount: value.Money{AmountMinor: 5, Currency: value.SGD},
		LifecycleState: "RECEIVED", ExecutionOutcome: "NOT_SUBMITTED", Version: 1, ReceivedAt: time.Now().UTC()}
	unprojected := id(t)
	pending, err := service.Payment(ctx, customer(mine), "Bearer customer", unprojected, 0)
	if err != nil || pending.Status != Pending || pending.PaymentID != unprojected || receipts.authorization != "Bearer customer" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	timeline, err := service.Timeline(ctx, customer(mine), "Bearer customer", unprojected)
	if err != nil || timeline.Status != Pending || len(timeline.Entries) != 0 {
		t.Fatalf("pending timeline: %+v %v", timeline, err)
	}
	receipts.err = ErrNotFound
	if _, err := service.Payment(ctx, customer(mine), "Bearer customer", unprojected, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown payment: %v", err)
	}
	receipts.err = nil

	timeline, err = service.Timeline(ctx, customer(mine), "Bearer customer", first)
	if err != nil || timeline.Status != Projected || len(timeline.Entries) != 2 || timeline.Entries[1].EventSequence != 2 {
		t.Fatalf("timeline: %+v %v", timeline, err)
	}

	// Customers must search within their scope; pages are newest first.
	if _, err := service.List(ctx, customer(mine), ListRequest{Limit: 2}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("customer search without customerId: %v", err)
	}
	if _, err := service.List(ctx, customer(mine), ListRequest{CustomerID: &theirs, Limit: 2}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign customer search: %v", err)
	}
	page, err := service.List(ctx, customer(mine), ListRequest{CustomerID: &mine, Limit: 2})
	if err != nil || len(page.Items) != 2 || page.Items[0].PaymentID != third || page.Items[1].PaymentID != second || page.NextCursor == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	next, err := service.List(ctx, customer(mine), ListRequest{CustomerID: &mine, Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].PaymentID != first || next.NextCursor != "" {
		t.Fatalf("second page: %+v %v", next, err)
	}
	validating := "VALIDATING"
	if _, err := service.List(ctx, customer(mine), ListRequest{CustomerID: &mine, Limit: 2, Cursor: page.NextCursor, LifecycleState: &validating}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a cursor must not be reused with other filters: %v", err)
	}
	if _, err := service.List(ctx, operator, ListRequest{CustomerID: &mine, Limit: 2, Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a cursor must not be reused by another caller: %v", err)
	}
	filtered, err := service.List(ctx, operator, ListRequest{CustomerID: &mine, LifecycleState: &validating, Limit: 10})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].PaymentID != first {
		t.Fatalf("operator filter: %+v %v", filtered, err)
	}
	if _, err := service.List(ctx, auth.Principal{Roles: map[string]struct{}{"compliance_reviewer": {}}}, ListRequest{Limit: 1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unrelated role: %v", err)
	}
}

func TestScratchQueryFreshness(t *testing.T) {
	store, service, _ := openScratch(t)
	ctx := context.Background()
	// Freshness follows the live consumer's broker heartbeat and offset lag.
	if err := store.Heartbeat(ctx, projections.PaymentConsumer); err != nil {
		t.Fatal(err)
	}
	service.now = time.Now
	meta, err := service.meta(ctx)
	if err != nil || meta.Generation < 1 {
		t.Fatalf("meta: %+v %v", meta, err)
	}
	if meta.ConsumerLag == 0 && meta.Freshness != Current {
		t.Fatalf("fresh heartbeat without lag: %+v", meta)
	}
	service.now = func() time.Time { return time.Now().Add(time.Minute) }
	if stale, err := service.meta(ctx); err != nil || stale.Freshness != Stale {
		t.Fatalf("heartbeat older than the window: %+v %v", stale, err)
	}
	service.now = time.Now
	if got := service.freshness(time.Now(), meta.BrokerSeenAt, 3); got != Lagging {
		t.Fatalf("backlog with a live broker: %s", got)
	}
	status, err := service.Status(ctx)
	if err != nil || len(status.Generations) == 0 || len(status.Consumers) == 0 {
		t.Fatalf("status: %+v %v", status, err)
	}
}
