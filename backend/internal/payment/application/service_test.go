package application_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/auth"
	"gtb.local/backend/internal/platform/value"
)

var (
	maya      = value.ID("20000000-0000-4000-8000-000000000001")
	daniel    = value.ID("20000000-0000-4000-8000-000000000002")
	mayaAcct  = value.ID("10000000-0000-4000-8000-000000000001")
	danAcct   = value.ID("10000000-0000-4000-8000-000000000002")
	clearing  = value.ID("10000000-0000-4000-8000-000000000003")
	missingID = value.ID("10000000-0000-4000-8000-00000000ffff")
)

// MemoryStore is a minimal in-memory Store for application and handler tests.
type MemoryStore struct {
	mu       sync.Mutex
	scopes   map[domain.IdempotencyScope]application.IdempotencyRecord
	records  map[value.ID]application.PaymentRecord
	commands map[value.ID]application.Command
	sequence int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		scopes:  map[domain.IdempotencyScope]application.IdempotencyRecord{},
		records: map[value.ID]application.PaymentRecord{}, commands: map[value.ID]application.Command{},
	}
}

func (store *MemoryStore) FindIdempotency(_ context.Context, scope domain.IdempotencyScope) (application.IdempotencyRecord, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.scopes[scope]
	return record, found, nil
}

func (store *MemoryStore) Receive(_ context.Context, receipt application.NewReceipt) (application.Receipt, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	paymentID, _ := value.NewID()
	commandID, _ := value.NewID()
	payment, _, err := domain.NewPayment(paymentID, receipt.Instruction, receipt.Actor, commandID, time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		return application.Receipt{}, err
	}
	dispatch := application.Dispatch{WorkflowID: domain.PaymentWorkflowID(paymentID), Status: "PENDING", UpdatedAt: payment.ReceivedAt}
	store.scopes[receipt.Scope] = application.IdempotencyRecord{RequestHash: payment.InstructionHash, PaymentID: paymentID, CommandID: commandID}
	store.records[paymentID] = application.PaymentRecord{Payment: payment, Instruction: receipt.Instruction, Dispatch: dispatch}
	store.commands[commandID] = application.Command{
		CommandID: commandID, CommandType: "SUBMIT_PAYMENT", PaymentID: paymentID, CustomerID: payment.CustomerID,
		Status: "COMMITTED", CommittedVersion: 1, CorrelationID: receipt.CorrelationID, RecordedAt: payment.ReceivedAt,
	}
	return application.Receipt{Payment: payment, CommandID: commandID, Dispatch: dispatch}, nil
}

func (store *MemoryStore) Receipt(_ context.Context, paymentID, commandID value.ID) (application.Receipt, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record := store.records[paymentID]
	return application.Receipt{Payment: record.Payment, CommandID: commandID, Dispatch: record.Dispatch}, nil
}

func (store *MemoryStore) Payment(_ context.Context, paymentID value.ID) (application.PaymentRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.records[paymentID]
	if !found {
		return application.PaymentRecord{}, application.ErrNotFound
	}
	return record, nil
}

func (store *MemoryStore) Command(_ context.Context, commandID value.ID) (application.Command, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	command, found := store.commands[commandID]
	if !found {
		return application.Command{}, application.ErrNotFound
	}
	return command, nil
}

type Directory struct {
	accounts map[value.ID]application.Account
	err      error
	calls    int
}

func NewDirectory() *Directory {
	customer := func(id, owner value.ID) application.Account {
		return application.Account{AccountID: id, CustomerID: &owner, Kind: "CUSTOMER_DEPOSIT", Currency: value.SGD, Status: "OPEN"}
	}
	return &Directory{accounts: map[value.ID]application.Account{
		mayaAcct: customer(mayaAcct, maya), danAcct: customer(danAcct, daniel),
		clearing: {AccountID: clearing, Kind: "OUTGOING_CLEARING", Currency: value.SGD, Status: "OPEN"},
	}}
}

func (directory *Directory) Account(_ context.Context, id value.ID) (application.Account, error) {
	directory.calls++
	if directory.err != nil {
		return application.Account{}, directory.err
	}
	account, found := directory.accounts[id]
	if !found {
		return application.Account{}, application.ErrAccountNotFound
	}
	return account, nil
}

func principal(roles []string, customers ...value.ID) auth.Principal {
	result := auth.Principal{Subject: "subject-1", Roles: map[string]struct{}{}, CustomerIDs: map[string]struct{}{}}
	for _, role := range roles {
		result.Roles[role] = struct{}{}
	}
	for _, customer := range customers {
		result.CustomerIDs[string(customer)] = struct{}{}
	}
	return result
}

func instruction(debtor, creditor value.ID, amount int64) domain.Instruction {
	return domain.Instruction{
		CustomerID: maya, Product: domain.BookTransfer, DebtorAccountID: debtor,
		Amount:   value.Money{AmountMinor: amount, Currency: value.SGD},
		Creditor: domain.Creditor{Name: "Daniel", AccountID: &creditor},
	}
}

func submit(service *application.Service, who auth.Principal, key string, instruction domain.Instruction) (application.Receipt, error) {
	return service.Submit(context.Background(), application.SubmitRequest{
		Principal: who, IdempotencyKey: key, Instruction: instruction, CorrelationID: "d6282a46-c2ce-4c47-9308-f0c7bf9aa031",
	})
}

func TestSubmitIdempotency(t *testing.T) {
	directory := NewDirectory()
	service := application.NewService(NewMemoryStore(), directory)
	customer := principal([]string{"customer"}, maya)

	first, err := submit(service, customer, "k-1", instruction(mayaAcct, danAcct, 100))
	if err != nil || first.Replayed || first.Payment.Lifecycle != domain.Received || first.Dispatch.Status != "PENDING" {
		t.Fatalf("first: %+v %v", first, err)
	}
	// Replays need no Core Banking call, so they succeed while it is unavailable.
	directory.err = application.ErrDependencyUnavailable
	second, err := submit(service, customer, "k-1", instruction(mayaAcct, danAcct, 100))
	if err != nil || !second.Replayed || second.Payment.ID != first.Payment.ID || second.CommandID != first.CommandID {
		t.Fatalf("replay: %+v %v", second, err)
	}
	if _, err := submit(service, customer, "k-1", instruction(mayaAcct, danAcct, 101)); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := submit(service, customer, "k-2", instruction(mayaAcct, danAcct, 100)); !errors.Is(err, application.ErrDependencyUnavailable) {
		t.Fatalf("new key while Core Banking is down: %v", err)
	}
}

func TestSubmitAuthorization(t *testing.T) {
	service := application.NewService(NewMemoryStore(), NewDirectory())
	customer := principal([]string{"customer"}, maya)
	for name, tc := range map[string]struct {
		who         auth.Principal
		instruction domain.Instruction
		want        error
	}{
		"foreign debtor account":   {customer, instruction(danAcct, mayaAcct, 100), application.ErrForbidden},
		"unknown debtor account":   {customer, instruction(missingID, danAcct, 100), application.ErrForbidden},
		"system debtor account":    {customer, instruction(clearing, danAcct, 100), application.ErrForbidden},
		"customer outside scope":   {principal([]string{"customer"}, daniel), instruction(mayaAcct, danAcct, 100), application.ErrForbidden},
		"operator cannot submit":   {principal([]string{"ops_viewer", "approver"}, maya), instruction(mayaAcct, danAcct, 100), application.ErrForbidden},
		"unknown creditor account": {customer, instruction(mayaAcct, missingID, 100), application.ErrInvalidRequest},
		"system creditor account":  {customer, instruction(mayaAcct, clearing, 100), application.ErrInvalidRequest},
		"invalid instruction":      {customer, instruction(mayaAcct, danAcct, 0), application.ErrInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := submit(service, tc.who, "k-"+name[:3], tc.instruction); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := submit(service, customer, "bad key", instruction(mayaAcct, danAcct, 100)); !errors.Is(err, application.ErrInvalidRequest) {
		t.Fatalf("invalid idempotency key: %v", err)
	}
	usd := instruction(mayaAcct, danAcct, 100)
	usd.Amount.Currency = value.USD
	if _, err := submit(service, customer, "usd", usd); !errors.Is(err, application.ErrInvalidRequest) {
		t.Fatalf("currency mismatch: %v", err)
	}
}

func TestReadsAreCustomerScoped(t *testing.T) {
	service := application.NewService(NewMemoryStore(), NewDirectory())
	receipt, err := submit(service, principal([]string{"customer"}, maya), "k-1", instruction(mayaAcct, danAcct, 100))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, tc := range map[string]struct {
		who  auth.Principal
		want error
	}{
		"owner":          {principal([]string{"customer"}, maya), nil},
		"ops viewer":     {principal([]string{"ops_viewer"}), nil},
		"auditor":        {principal([]string{"auditor"}), nil},
		"other customer": {principal([]string{"customer"}, daniel), application.ErrForbidden},
		"scope no role":  {principal(nil, maya), application.ErrForbidden},
		"compliance":     {principal([]string{"compliance_reviewer"}), application.ErrForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Payment(ctx, tc.who, receipt.Payment.ID); !errors.Is(err, tc.want) {
				t.Fatalf("payment: %v", err)
			}
			if _, err := service.Command(ctx, tc.who, receipt.CommandID); !errors.Is(err, tc.want) {
				t.Fatalf("command: %v", err)
			}
		})
	}
	unknown := value.ID("70000000-0000-4000-8000-00000000ffff")
	if _, err := service.Payment(ctx, principal([]string{"ops_viewer"}), unknown); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("unknown payment: %v", err)
	}
	if _, err := service.Command(ctx, principal([]string{"ops_viewer"}), "not-a-uuid"); !errors.Is(err, application.ErrInvalidRequest) {
		t.Fatalf("invalid command ID: %v", err)
	}
}
