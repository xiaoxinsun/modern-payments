package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
)

var (
	customerID = value.ID("20000000-0000-4000-8000-000000000001")
	paymentID  = value.ID("70000000-0000-4000-8000-000000000001")
	commandID  = value.ID("71000000-0000-4000-8000-000000000001")
	claimID    = value.ID("72000000-0000-4000-8000-000000000001")
	now        = time.Date(2026, 9, 26, 13, 41, 23, 685194000, time.UTC)
)

// fakeService returns fixed values, or err for every call when set.
type fakeService struct {
	err      error
	replayed bool
	record   application.PaymentRecord
	submits  int
}

func (fake *fakeService) Submit(_ context.Context, request application.SubmitRequest) (application.Receipt, error) {
	fake.submits++
	if fake.err != nil {
		return application.Receipt{}, fake.err
	}
	if request.IdempotencyKey == "" || request.CorrelationID.Validate() != nil || request.Principal.Subject == "" {
		return application.Receipt{}, errors.New("handler did not pass request context")
	}
	return application.Receipt{Payment: fake.record.Payment, CommandID: commandID, Dispatch: fake.record.Dispatch, Replayed: fake.replayed}, nil
}

func (fake *fakeService) Payment(context.Context, auth.Principal, value.ID) (application.PaymentRecord, error) {
	return fake.record, fake.err
}

func (fake *fakeService) Command(context.Context, auth.Principal, value.ID) (application.Command, error) {
	return application.Command{
		CommandID: commandID, CommandType: "SUBMIT_PAYMENT", PaymentID: paymentID, CustomerID: customerID,
		Status: "COMMITTED", CommittedVersion: 1, CorrelationID: claimID, RecordedAt: now,
	}, fake.err
}

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

func record() application.PaymentRecord {
	creditor := value.ID("10000000-0000-4000-8000-000000000002")
	return application.PaymentRecord{
		Payment: domain.Payment{
			ID: paymentID, CustomerID: customerID, Product: domain.BookTransfer, Lifecycle: domain.Accepted,
			Outcome: domain.NotSubmitted, Version: 4, ExecutionWorkflowID: domain.ExecutionWorkflowID(paymentID, domain.BookTransfer),
			Claim:      &domain.Claim{Kind: domain.SubmissionClaim, ClaimID: claimID, ClaimedAt: now},
			ReceivedAt: now, UpdatedAt: now,
		},
		Instruction: domain.Instruction{
			CustomerID: customerID, Product: domain.BookTransfer, DebtorAccountID: "10000000-0000-4000-8000-000000000001",
			Amount:   value.Money{AmountMinor: 10000, Currency: value.SGD},
			Creditor: domain.Creditor{Name: "Daniel", AccountID: &creditor}, Reference: "rent",
		},
		Dispatch: application.Dispatch{WorkflowID: domain.PaymentWorkflowID(paymentID), Status: "PENDING", UpdatedAt: now},
	}
}

func newServer(t *testing.T, service PaymentService) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	verifier := tokenVerifier{"customer": role("customer"), "ops": role("ops_viewer"), "compliance": role("compliance_reviewer")}
	if err := Register(mux, verifier, service); err != nil {
		t.Fatal(err)
	}
	return platformruntime.WithCorrelation(mux)
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

const validBody = `{"customerId":"20000000-0000-4000-8000-000000000001","product":"BOOK_TRANSFER",
"debtorAccountId":"10000000-0000-4000-8000-000000000001","amount":{"amountMinor":"10000","currency":"SGD"},
"creditor":{"name":"Daniel","accountId":"10000000-0000-4000-8000-000000000002"},"reference":"rent"}`

func call(handler http.Handler, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// validateResponse checks status, headers and body against the contract.
func validateResponse(t *testing.T, router routers.Router, method, path string, response *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	route, parameters, err := router.FindRoute(request)
	if err != nil {
		t.Fatal(err)
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: request, PathParams: parameters, Route: route},
		Status:                 response.Code, Header: response.Header(),
		Body:    io.NopCloser(bytes.NewReader(response.Body.Bytes())),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s -> %d does not match contract: %v\n%s", method, path, response.Code, err, response.Body.String())
	}
}

func TestResponsesMatchOpenAPIContract(t *testing.T) {
	router := contractRouter(t)
	service := &fakeService{record: record()}
	handler := newServer(t, service)
	headers := map[string]string{"Idempotency-Key": "order-1"}

	submitted := call(handler, http.MethodPost, "/api/v1/payments", "customer", validBody, headers)
	if submitted.Code != http.StatusAccepted || submitted.Header().Get("Idempotent-Replayed") != "false" {
		t.Fatalf("submit: %d %s", submitted.Code, submitted.Body)
	}
	validateResponse(t, router, http.MethodPost, "/api/v1/payments", submitted)
	service.replayed = true
	replayed := call(handler, http.MethodPost, "/api/v1/payments", "customer", validBody, headers)
	if replayed.Code != http.StatusAccepted || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d", replayed.Code)
	}
	receiptPath := "/api/v1/payments/" + string(paymentID) + "/receipt"
	for _, token := range []string{"customer", "ops"} {
		response := call(handler, http.MethodGet, receiptPath, token, "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("receipt as %s: %d", token, response.Code)
		}
		validateResponse(t, router, http.MethodGet, receiptPath, response)
	}
	commandPath := "/api/v1/commands/" + string(commandID)
	response := call(handler, http.MethodGet, commandPath, "customer", "", nil)
	validateResponse(t, router, http.MethodGet, commandPath, response)

	for err, status := range map[error]int{
		application.ErrForbidden:             http.StatusForbidden,
		application.ErrIdempotencyConflict:   http.StatusConflict,
		application.ErrDependencyUnavailable: http.StatusServiceUnavailable,
		fmt.Errorf("%w: %w", application.ErrInvalidRequest, domain.ErrInvalidInstruction): http.StatusBadRequest,
		errors.New("unexpected"): http.StatusInternalServerError,
	} {
		failing := newServer(t, &fakeService{err: err})
		response := call(failing, http.MethodPost, "/api/v1/payments", "customer", validBody, headers)
		if response.Code != status {
			t.Fatalf("%v: got %d, want %d", err, response.Code, status)
		}
		validateResponse(t, router, http.MethodPost, "/api/v1/payments", response)
	}
	missing := call(newServer(t, &fakeService{err: application.ErrNotFound}), http.MethodGet, receiptPath, "ops", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("not found: %d", missing.Code)
	}
	validateResponse(t, router, http.MethodGet, receiptPath, missing)
}

func TestRequestsAreValidatedAndAuthorizedBeforeTheService(t *testing.T) {
	router := contractRouter(t)
	service := &fakeService{record: record()}
	handler := newServer(t, service)
	key := map[string]string{"Idempotency-Key": "order-1"}
	for name, tc := range map[string]struct {
		method, path, token, body string
		headers                   map[string]string
		want                      int
	}{
		"missing token":          {http.MethodPost, "/api/v1/payments", "", validBody, key, http.StatusUnauthorized},
		"operator submit":        {http.MethodPost, "/api/v1/payments", "ops", validBody, key, http.StatusForbidden},
		"compliance read":        {http.MethodGet, "/api/v1/payments/" + string(paymentID) + "/receipt", "compliance", "", nil, http.StatusForbidden},
		"missing idempotency":    {http.MethodPost, "/api/v1/payments", "customer", validBody, nil, http.StatusBadRequest},
		"bad idempotency key":    {http.MethodPost, "/api/v1/payments", "customer", validBody, map[string]string{"Idempotency-Key": "has space"}, http.StatusBadRequest},
		"unknown field":          {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"reference"`, `"extra":1,"reference"`, 1), key, http.StatusBadRequest},
		"noncanonical amount":    {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"10000"`, `"010000"`, 1), key, http.StatusBadRequest},
		"numeric amount":         {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"10000"`, `10000`, 1), key, http.StatusBadRequest},
		"zero amount":            {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"10000"`, `"0"`, 1), key, http.StatusBadRequest},
		"unsupported currency":   {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"SGD"`, `"JPY"`, 1), key, http.StatusBadRequest},
		"trailing JSON":          {http.MethodPost, "/api/v1/payments", "customer", validBody + `{}`, key, http.StatusBadRequest},
		"oversized body":         {http.MethodPost, "/api/v1/payments", "customer", strings.Replace(validBody, `"rent"`, `"`+strings.Repeat("x", 70*1024)+`"`, 1), key, http.StatusBadRequest},
		"invalid payment ID":     {http.MethodGet, "/api/v1/payments/not-a-uuid/receipt", "customer", "", nil, http.StatusBadRequest},
		"invalid correlation ID": {http.MethodGet, "/api/v1/commands/" + string(commandID), "customer", "", map[string]string{"X-Correlation-ID": "nope"}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			before := service.submits
			response := call(handler, tc.method, tc.path, tc.token, tc.body, tc.headers)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.want, response.Body)
			}
			if service.submits != before {
				t.Fatal("invalid or unauthorized request reached the service")
			}
			validateResponse(t, router, tc.method, strings.Replace(tc.path, "not-a-uuid", string(paymentID), 1), response)
		})
	}
}

func TestEmbeddedBundleMatchesContract(t *testing.T) {
	fresh, err := contracts.BundleOpenAPI("../../../../contracts/openapi/v1/payment.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, bundle) {
		t.Fatal("payment.bundle.json is stale; run make openapi-generate")
	}
}
