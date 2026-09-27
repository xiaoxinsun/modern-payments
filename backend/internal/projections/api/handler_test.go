package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgtype"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/query"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

var (
	customerID = value.ID("20000000-0000-4000-8000-000000000001")
	paymentID  = value.ID("70000000-0000-4000-8000-000000000001")
	now        = time.Date(2026, 9, 27, 8, 1, 2, 345678000, time.UTC)
)

type tokenVerifier map[string]auth.Principal

func (verifier tokenVerifier) Verify(_ context.Context, token string) (auth.Principal, error) {
	principal, ok := verifier[token]
	if !ok {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	return principal, nil
}

func role(name string) auth.Principal {
	return auth.Principal{Subject: name, Roles: map[string]struct{}{name: {}}, CustomerIDs: map[string]struct{}{string(customerID): {}}}
}

type fakeQueries struct {
	err           error
	status        query.ViewStatus
	authorization string
	list          query.ListRequest
}

func testMeta() query.Meta {
	return query.Meta{AsOf: now, Generation: 2, Freshness: query.Current, BrokerSeenAt: &now, LastEventAt: &now}
}

func summary() query.Summary {
	return query.Summary{
		PaymentID: paymentID, CustomerID: customerID, Product: "BOOK_TRANSFER",
		Amount: &value.Money{AmountMinor: 2500, Currency: value.SGD}, LifecycleState: "RECEIVED",
		ExecutionOutcome: "NOT_SUBMITTED", Version: 1, ReceivedAt: now, LastEventAt: &now, ProjectedAt: &now,
	}
}

func (fake *fakeQueries) Payment(_ context.Context, _ auth.Principal, authorization string, _ value.ID, _ int64) (query.View, error) {
	fake.authorization = authorization
	view := query.View{Summary: summary(), Status: fake.status, Meta: testMeta()}
	if fake.status == query.Pending {
		view.LastEventAt, view.ProjectedAt = nil, nil
	}
	return view, fake.err
}

func (fake *fakeQueries) Timeline(context.Context, auth.Principal, string, value.ID) (query.Timeline, error) {
	return query.Timeline{PaymentID: paymentID, Status: query.Projected, Meta: testMeta(), Entries: []query.TimelineEntry{{
		EventID: "30000000-0000-4000-8000-000000000001", EventSequence: 1, EventType: "PaymentReceived", AggregateVersion: 1,
		LifecycleState: "RECEIVED", ExecutionOutcome: "NOT_SUBMITTED", OccurredAt: now,
		CorrelationID: "30000000-0000-4000-8000-000000000002", CausationID: "30000000-0000-4000-8000-000000000003",
	}}}, fake.err
}

func (fake *fakeQueries) List(_ context.Context, _ auth.Principal, request query.ListRequest) (query.Page, error) {
	fake.list = request
	return query.Page{Items: []query.Summary{summary()}, NextCursor: "opaque", Meta: testMeta()}, fake.err
}

func (fake *fakeQueries) Status(context.Context) (query.Status, error) {
	watermark := int64(40)
	return query.Status{
		AsOf: now,
		Generations: []projectiondb.ListGenerationsRow{{
			Generation: 2, Status: "ACTIVE", JournalWatermark: &watermark,
			CreatedAt: timestamptz(now), ActivatedAt: timestamptz(now),
		}},
		Consumers: []query.ConsumerStatus{{Consumer: "payment-projector", Freshness: query.Lagging, BrokerSeenAt: &now,
			Partitions: []projectiondb.AppConsumerOffset{{Consumer: "payment-projector", Topic: "gtb.payment.events.v1", Partition: 1,
				NextOffset: 7, HighWatermark: 9, UpdatedAt: timestamptz(now)}}}},
		Buffered: query.Backlog{Count: 1, OldestAt: &now},
	}, fake.err
}

func newServer(t *testing.T, service QueryService) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	verifier := tokenVerifier{}
	for _, name := range []string{"customer", "ops_viewer", "auditor", "platform_admin", "compliance_reviewer", "internal_service"} {
		verifier[name] = role(name)
	}
	if err := Register(mux, verifier, service); err != nil {
		t.Fatal(err)
	}
	return platformruntime.WithCorrelation(mux)
}

func call(handler http.Handler, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func contractRouter(t *testing.T) routers.Router {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromData(bundle)
	if err != nil {
		t.Fatal(err)
	}
	document.Servers = nil
	router, err := legacy.NewRouter(document)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func validateResponse(t *testing.T, router routers.Router, path string, response *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	route, parameters, err := router.FindRoute(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: request, PathParams: parameters, Route: route},
		Status:                 response.Code, Header: response.Header(), Body: io.NopCloser(bytes.NewReader(response.Body.Bytes())),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}); err != nil {
		t.Fatalf("GET %s -> %d does not match contract: %v\n%s", path, response.Code, err, response.Body.String())
	}
}

func TestResponsesMatchQueryContract(t *testing.T) {
	router := contractRouter(t)
	payment := "/api/v1/payments/" + string(paymentID)
	for _, tc := range []struct {
		path, token string
		status      query.ViewStatus
	}{
		{payment, "customer", query.Projected},
		{payment + "?minVersion=3", "ops_viewer", query.Behind},
		{payment, "customer", query.Pending},
		{payment + "/timeline", "auditor", query.Projected},
		{"/api/v1/payments?customerId=" + string(customerID) + "&lifecycleState=RECEIVED&limit=10&cursor=abc", "customer", ""},
		{"/api/v1/ops/projections", "platform_admin", ""},
	} {
		fake := &fakeQueries{status: tc.status}
		response := call(newServer(t, fake), tc.path, tc.token)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body)
		}
		validateResponse(t, router, tc.path, response)
		if tc.status != "" && fake.authorization != "Bearer "+tc.token && tc.path != payment+"/timeline" {
			t.Fatalf("%s: the caller's token must be available for the receipt fallback", tc.path)
		}
	}
	for err, status := range map[error]int{
		query.ErrForbidden: http.StatusForbidden, query.ErrNotFound: http.StatusNotFound,
		query.ErrInvalidRequest: http.StatusBadRequest, query.ErrDependencyUnavailable: http.StatusServiceUnavailable,
		errors.New("unexpected"): http.StatusInternalServerError,
	} {
		response := call(newServer(t, &fakeQueries{err: err}), payment, "customer")
		if response.Code != status {
			t.Fatalf("%v: %d, want %d", err, response.Code, status)
		}
		validateResponse(t, router, payment, response)
	}
}

func TestQueryRequestsAreValidatedAndAuthorized(t *testing.T) {
	payment := "/api/v1/payments/" + string(paymentID)
	for name, tc := range map[string]struct {
		path, token string
		want        int
	}{
		"missing token":             {payment, "", http.StatusUnauthorized},
		"compliance role":           {payment, "compliance_reviewer", http.StatusForbidden},
		"service identity":          {"/api/v1/payments", "internal_service", http.StatusForbidden},
		"customer reads ops status": {"/api/v1/ops/projections", "customer", http.StatusForbidden},
		"limit above maximum":       {"/api/v1/payments?limit=101", "ops_viewer", http.StatusBadRequest},
		"unknown lifecycle state":   {"/api/v1/payments?lifecycleState=TELEPORTED", "ops_viewer", http.StatusBadRequest},
		"non-canonical customer ID": {"/api/v1/payments?customerId=abc", "ops_viewer", http.StatusBadRequest},
		"zero minVersion":           {payment + "?minVersion=0", "ops_viewer", http.StatusBadRequest},
		"invalid payment ID":        {"/api/v1/payments/not-a-uuid", "ops_viewer", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			response := call(newServer(t, &fakeQueries{status: query.Projected}), tc.path, tc.token)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.want, response.Body)
			}
		})
	}
	fake := &fakeQueries{}
	call(newServer(t, fake), "/api/v1/payments?customerId="+string(customerID)+"&limit=7", "customer")
	if fake.list.Limit != 7 || fake.list.CustomerID == nil || *fake.list.CustomerID != customerID {
		t.Fatalf("list parameters not passed: %+v", fake.list)
	}
	call(newServer(t, fake), "/api/v1/payments", "ops_viewer")
	if fake.list.Limit != 50 {
		t.Fatalf("default limit: %d", fake.list.Limit)
	}
}

func TestEmbeddedQueryBundleMatchesContract(t *testing.T) {
	fresh, err := contracts.BundleOpenAPI("../../../../contracts/openapi/v1/query.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, bundle) {
		t.Fatal("query.bundle.json is stale; run make openapi-generate")
	}
}

func timestamptz(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
