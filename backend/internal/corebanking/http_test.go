package corebanking

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"gtb.local/backend/internal/corebanking/domain"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
)

const contractRoot = "https://gtb.local/contracts/"

// fakeBanking returns fixed domain values, or err for every call when set.
type fakeBanking struct {
	err     error
	account domain.Account
	hold    domain.Hold
	posting domain.Posting
	op      domain.Operation
	lines   []domain.StatementEntry
}

func (f fakeBanking) GetAccount(context.Context, value.ID) (domain.Account, error) {
	return f.account, f.err
}
func (f fakeBanking) GetOperation(context.Context, value.ID) (domain.Operation, error) {
	return f.op, f.err
}
func (f fakeBanking) GetHold(context.Context, value.ID) (domain.Hold, error) { return f.hold, f.err }
func (f fakeBanking) Reserve(context.Context, ReserveRequest) (domain.Hold, error) {
	return f.hold, f.err
}
func (f fakeBanking) Release(context.Context, ReleaseRequest) (domain.Hold, error) {
	return f.hold, f.err
}
func (f fakeBanking) Post(context.Context, PostRequest) (domain.Posting, error) {
	return f.posting, f.err
}
func (f fakeBanking) Reverse(context.Context, ReverseRequest) (domain.Posting, error) {
	return f.posting, f.err
}
func (f fakeBanking) Statement(context.Context, value.ID, *time.Time, *time.Time, int32) ([]domain.StatementEntry, error) {
	return f.lines, f.err
}

func contractSchema(t *testing.T, pointer string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	for _, path := range []string{"common/v1/types.schema.json", "openapi/v1/shared.json", "openapi/v1/core-banking.json"} {
		data, err := os.ReadFile("../../../contracts/" + path)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(contractRoot+path, document); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(contractRoot + pointer)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func testID(t *testing.T) value.ID {
	t.Helper()
	id, err := value.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHTTPResponsesMatchOpenAPIContract(t *testing.T) {
	now := time.Date(2026, 9, 26, 13, 41, 23, 685194000, time.UTC)
	account, hold, operation, posting := testID(t), testID(t), testID(t), testID(t)
	sgd := func(minor int64) value.Money { return value.Money{AmountMinor: minor, Currency: value.SGD} }
	entries := []domain.Entry{
		{AccountID: account, Side: domain.Debit, Amount: sgd(10000)},
		{AccountID: testID(t), Side: domain.Credit, Amount: sgd(10000)},
	}
	fake := fakeBanking{
		account: domain.Account{
			AccountID: account, ExternalReference: "customer/maya/sgd", CustomerID: &account,
			Kind: domain.CustomerDeposit, Currency: value.SGD, BookedMinor: 100000,
			AvailableMinor: 90000, Status: "OPEN", CreatedAt: now, UpdatedAt: now,
		},
		hold: domain.Hold{
			HoldID: hold, AccountID: account, ReserveOperationID: operation, Amount: sgd(10000),
			Status: domain.HoldConsumed, ExpiresAt: &now, ConsumedByOperationID: &operation,
			CreatedAt: now, UpdatedAt: now,
		},
		posting: domain.Posting{
			PostingID: posting, OperationID: operation, Description: "B compensating reversal",
			ReversalOfPostingID: &posting, Entries: entries, EventID: testID(t), CommittedAt: now,
		},
		op: domain.Operation{
			OperationID: operation, OperationType: "POST", ResourceType: "POSTING",
			ResourceID: posting, CorrelationID: testID(t), CommittedAt: now,
		},
		lines: []domain.StatementEntry{{
			PostingID: posting, OperationID: operation, Description: "A2 customer debit",
			CommittedAt: now, Side: domain.Debit, Amount: sgd(10000),
		}},
	}
	handler := platformruntime.WithCorrelation(NewHTTPHandler(fake))
	base := "/integrations/v1/core-banking"
	postBody := `{"operationId":"` + string(operation) + `","description":"A2","entries":[` +
		`{"accountId":"` + string(account) + `","side":"DEBIT","amount":{"amountMinor":"10000","currency":"SGD"}},` +
		`{"accountId":"` + string(entries[1].AccountID) + `","side":"CREDIT","amount":{"amountMinor":"10000","currency":"SGD"}}]}`
	for _, tc := range []struct {
		method, path, body, schema string
	}{
		{"GET", base + "/accounts/" + string(account), "", "Account"},
		{"GET", base + "/accounts/" + string(account) + "/statement?limit=10", "", "Statement"},
		{"GET", base + "/operations/" + string(operation), "", "Operation"},
		{"GET", base + "/holds/" + string(hold), "", "Hold"},
		{"POST", base + "/holds", `{"operationId":"` + string(operation) + `","accountId":"` + string(account) + `","amount":{"amountMinor":"10000","currency":"SGD"}}`, "Hold"},
		{"POST", base + "/holds/" + string(hold) + "/releases", `{"operationId":"` + string(operation) + `"}`, "Hold"},
		{"POST", base + "/postings", postBody, "Posting"},
		{"POST", base + "/reversals", `{"operationId":"` + string(operation) + `","originalPostingId":"` + string(posting) + `","description":"B"}`, "Posting"},
	} {
		t.Run(tc.method+" "+tc.schema, func(t *testing.T) {
			if tc.body != "" {
				var request any
				if err := json.Unmarshal([]byte(tc.body), &request); err != nil {
					t.Fatal(err)
				}
				requestSchema := map[string]string{"/holds": "ReserveRequest", "/releases": "ReleaseRequest", "/postings": "PostRequest", "/reversals": "ReverseRequest"}
				for suffix, name := range requestSchema {
					if strings.HasSuffix(tc.path, suffix) {
						if err := contractSchema(t, "openapi/v1/core-banking.json#/components/schemas/"+name).Validate(request); err != nil {
							t.Fatalf("request fixture violates %s: %v", name, err)
						}
					}
				}
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
			}
			var body any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if err := contractSchema(t, "openapi/v1/core-banking.json#/components/schemas/"+tc.schema).Validate(body); err != nil {
				t.Fatalf("response violates %s: %v\n%s", tc.schema, err, recorder.Body)
			}
		})
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	envelope := contractSchema(t, "common/v1/types.schema.json#/$defs/ErrorEnvelope")
	operation := testID(t)
	reserve := `{"operationId":"` + string(operation) + `","accountId":"` + string(testID(t)) + `","amount":{"amountMinor":"1","currency":"SGD"}}`
	for _, tc := range []struct {
		name   string
		err    error
		body   string
		status int
		code   string
	}{
		{"conflicting operation reuse", ErrConflict, reserve, http.StatusConflict, "CONFLICT"},
		{"insufficient funds", ErrInsufficientFunds, reserve, http.StatusConflict, "CONFLICT"},
		{"expired hold", ErrExpiredHold, reserve, http.StatusConflict, "CONFLICT"},
		{"missing record", ErrNotFound, reserve, http.StatusNotFound, "NOT_FOUND"},
		{"invalid request", invalidRequest("reservation amount must be positive and valid"), reserve, http.StatusBadRequest, "INVALID_REQUEST"},
		{"unknown field", nil, strings.TrimSuffix(reserve, "}") + `,"overdraft":true}`, http.StatusBadRequest, "INVALID_REQUEST"},
		{"trailing document", nil, reserve + reserve, http.StatusBadRequest, "INVALID_REQUEST"},
		{"unexpected failure", errors.New("boom"), reserve, http.StatusInternalServerError, "INTERNAL_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := platformruntime.WithCorrelation(NewHTTPHandler(fakeBanking{err: tc.err}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest("POST", "/integrations/v1/core-banking/holds", strings.NewReader(tc.body)))
			if recorder.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", recorder.Code, tc.status, recorder.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if err := envelope.Validate(body); err != nil {
				t.Fatalf("error envelope: %v", err)
			}
			if code := body["error"].(map[string]any)["code"]; code != tc.code {
				t.Fatalf("code %v, want %s", code, tc.code)
			}
			if strings.Contains(recorder.Body.String(), "boom") {
				t.Fatal("internal error detail leaked to caller")
			}
		})
	}
}
