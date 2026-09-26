package corebanking

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/platform/value"
)

const account = value.ID("10000000-0000-4000-8000-000000000001")

func TestAccountLookupMapsOnlyDefinitiveAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"found":          {http.StatusOK, `{"accountId":"` + string(account) + `","customerId":"20000000-0000-4000-8000-000000000001","accountKind":"CUSTOMER_DEPOSIT","currency":"SGD","status":"OPEN"}`, nil},
		"not found":      {http.StatusNotFound, `{}`, application.ErrAccountNotFound},
		"token rejected": {http.StatusUnauthorized, `{}`, application.ErrDependencyUnavailable},
		"server error":   {http.StatusServiceUnavailable, `{}`, application.ErrDependencyUnavailable},
		"other account":  {http.StatusOK, `{"accountId":"10000000-0000-4000-8000-000000000002"}`, application.ErrDependencyUnavailable},
		"malformed":      {http.StatusOK, `not json`, application.ErrDependencyUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/integrations/v1/core-banking/accounts/"+string(account) {
					t.Errorf("unexpected path %s", request.URL.Path)
				}
				writer.WriteHeader(tc.status)
				_, _ = writer.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Account(context.Background(), account)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.want == nil && (got.CustomerID == nil || got.Kind != "CUSTOMER_DEPOSIT" || got.Currency != value.SGD) {
				t.Fatalf("unexpected account %+v", got)
			}
		})
	}
	unreachable, _ := NewClient("http://127.0.0.1:1", http.DefaultClient)
	if _, err := unreachable.Account(context.Background(), account); !errors.Is(err, application.ErrDependencyUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	if _, err := NewClient("http://user:secret@example.test", http.DefaultClient); err == nil {
		t.Fatal("accepted credentials in Core Banking URL")
	}
}
