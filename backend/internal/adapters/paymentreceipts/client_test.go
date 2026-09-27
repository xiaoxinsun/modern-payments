package paymentreceipts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/query"
)

const paymentID = value.ID("30000000-0000-4000-8000-000000000002")

func TestReceiptForwardsTheCallersTokenAndMapsStatuses(t *testing.T) {
	var authorization string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if request.URL.Path != "/api/v1/payments/"+string(paymentID)+"/receipt" {
			t.Errorf("unexpected path %s", request.URL.Path)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"paymentId":"` + string(paymentID) + `","customerId":"20000000-0000-4000-8000-000000000001",
"version":"1","lifecycleState":"RECEIVED","executionOutcome":"NOT_SUBMITTED",
"instruction":{"product":"BOOK_TRANSFER","amount":{"amountMinor":"2500","currency":"SGD"}},"receivedAt":"2026-09-27T01:02:03Z"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Receipt(context.Background(), "Bearer caller", paymentID)
	if err != nil || authorization != "Bearer caller" || receipt.Version != 1 || receipt.Amount.AmountMinor != 2500 || receipt.Product != "BOOK_TRANSFER" {
		t.Fatalf("receipt: %+v %v (authorization %q)", receipt, err, authorization)
	}
	for code, want := range map[int]error{
		http.StatusNotFound: query.ErrNotFound, http.StatusForbidden: query.ErrForbidden,
		http.StatusUnauthorized: query.ErrDependencyUnavailable, http.StatusServiceUnavailable: query.ErrDependencyUnavailable,
	} {
		status = code
		if _, err := client.Receipt(context.Background(), "Bearer caller", paymentID); !errors.Is(err, want) {
			t.Fatalf("HTTP %d: %v, want %v", code, err, want)
		}
	}
	unreachable, _ := NewClient("http://127.0.0.1:1", http.DefaultClient)
	if _, err := unreachable.Receipt(context.Background(), "Bearer caller", paymentID); !errors.Is(err, query.ErrDependencyUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}
