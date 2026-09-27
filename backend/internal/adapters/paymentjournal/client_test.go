package paymentjournal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const entry = `{"position":"7","event":{"eventId":"30000000-0000-4000-8000-000000000001","eventType":"PaymentReceived",
"schemaVersion":1,"aggregateType":"Payment","aggregateId":"30000000-0000-4000-8000-000000000002","aggregateVersion":"1",
"eventSequence":"1","occurredAt":"2026-09-27T01:02:03Z","recordedAt":"2026-09-27T01:02:03Z",
"correlationId":"30000000-0000-4000-8000-000000000003","causationId":"30000000-0000-4000-8000-000000000003",
"source":"payment-api","payload":{"paymentId":"30000000-0000-4000-8000-000000000002"}}}`

func TestJournalClientDecodesPagesAndWatermark(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.String())
		switch request.URL.Path {
		case "/integrations/v1/payment-events/watermark":
			_, _ = writer.Write([]byte(`{"position":"7","events":"7","payments":"3"}`))
		default:
			_, _ = writer.Write([]byte(`{"entries":[` + entry + `]}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := client.Watermark(context.Background())
	if err != nil || watermark.Position != 7 || watermark.Payments != 3 {
		t.Fatalf("watermark: %+v %v", watermark, err)
	}
	page, err := client.Page(context.Background(), 2, 7, 100)
	if err != nil || len(page) != 1 || page[0].Position != 7 || page[0].Event.Validate() != nil {
		t.Fatalf("page: %+v %v", page, err)
	}
	events, err := client.PaymentEvents(context.Background(), "30000000-0000-4000-8000-000000000002", 0, 50)
	if err != nil || len(events) != 1 {
		t.Fatalf("payment events: %+v %v", events, err)
	}
	want := []string{
		"/integrations/v1/payment-events/watermark",
		"/integrations/v1/payment-events?afterPosition=2&limit=100&upToPosition=7",
		"/integrations/v1/payments/30000000-0000-4000-8000-000000000002/events?afterSequence=0&limit=50",
	}
	for index := range want {
		if paths[index] != want[index] {
			t.Fatalf("request %d: %s, want %s", index, paths[index], want[index])
		}
	}
}

func TestJournalClientTreatsAnythingElseAsUnavailable(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"forbidden": func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusForbidden) },
		"malformed": func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"entries":[{"position":"x"}]}`))
		},
		"unreadable": func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`not json`)) },
	} {
		server := httptest.NewServer(handler)
		client, _ := NewClient(server.URL, server.Client())
		if _, err := client.Page(context.Background(), 0, 1, 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", name, err)
		}
		server.Close()
	}
	if _, err := NewClient("http://user:secret@example.test", http.DefaultClient); err == nil {
		t.Fatal("credentials in the URL must be rejected")
	}
}
