package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/auth"
	"gtb.local/backend/internal/platform/value"
)

var (
	ErrInvalidRequest        = errors.New("invalid payment request")
	ErrForbidden             = errors.New("payment access denied")
	ErrNotFound              = errors.New("payment record not found")
	ErrIdempotencyConflict   = errors.New("idempotency key was reused with a different payload")
	ErrDependencyUnavailable = errors.New("payment dependency unavailable")
	ErrAccountNotFound       = errors.New("account not found")
)

const CustomerRole = "customer"

// OperatorReadRoles may inspect any receipt; they cannot submit customer payments.
var OperatorReadRoles = []string{"ops_viewer", "investigator", "approver", "auditor"}

// Account is the Core Banking account view needed for authorization. The
// payment service never reads Core Banking tables; it asks the owner.
type Account struct {
	AccountID  value.ID
	CustomerID *value.ID
	Kind       string
	Currency   value.Currency
	Status     string
}

type AccountDirectory interface {
	// Account returns ErrAccountNotFound for unknown accounts and
	// ErrDependencyUnavailable when ownership cannot be established.
	Account(context.Context, value.ID) (Account, error)
}

type IdempotencyRecord struct {
	RequestHash string
	PaymentID   value.ID
	CommandID   value.ID
}

type NewReceipt struct {
	Scope         domain.IdempotencyScope
	Instruction   domain.Instruction
	Actor         domain.Actor
	CorrelationID value.ID
}

type Dispatch struct {
	WorkflowID string
	Status     string
	Attempts   int32
	UpdatedAt  time.Time
}

type Receipt struct {
	Payment   domain.Payment
	CommandID value.ID
	Dispatch  Dispatch
	Replayed  bool
}

type PaymentRecord struct {
	Payment     domain.Payment
	Instruction domain.Instruction
	Dispatch    Dispatch
}

type Command struct {
	CommandID        value.ID
	CommandType      string
	PaymentID        value.ID
	CustomerID       value.ID
	Status           string
	CommittedVersion int64
	CorrelationID    value.ID
	RecordedAt       time.Time
}

// Store persists the payment authority. Receive commits the instruction,
// idempotency record, command, audit, event/outbox and dispatch rows atomically
// and resolves concurrent duplicates by replay or ErrIdempotencyConflict.
type Store interface {
	FindIdempotency(context.Context, domain.IdempotencyScope) (IdempotencyRecord, bool, error)
	Receive(context.Context, NewReceipt) (Receipt, error)
	Receipt(context.Context, value.ID, value.ID) (Receipt, error)
	Payment(context.Context, value.ID) (PaymentRecord, error)
	Command(context.Context, value.ID) (Command, error)
}

type Service struct {
	store    Store
	accounts AccountDirectory
}

func NewService(store Store, accounts AccountDirectory) *Service {
	return &Service{store: store, accounts: accounts}
}

type SubmitRequest struct {
	Principal      auth.Principal
	IdempotencyKey string
	Instruction    domain.Instruction
	CorrelationID  value.ID
}

// Submit durably receives a customer instruction. Success means received with a
// pending workflow dispatch, never accepted or completed.
func (service *Service) Submit(ctx context.Context, request SubmitRequest) (Receipt, error) {
	instruction := request.Instruction
	if !request.Principal.HasRole(CustomerRole) || !request.Principal.CanAccessCustomer(string(instruction.CustomerID)) {
		return Receipt{}, fmt.Errorf("%w: customer scope denied", ErrForbidden)
	}
	scope := domain.IdempotencyScope{Channel: domain.CustomerAPI, CustomerID: instruction.CustomerID, Key: request.IdempotencyKey}
	if err := scope.Validate(); err != nil {
		return Receipt{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	hash, err := instruction.Hash()
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err := request.CorrelationID.Validate(); err != nil {
		return Receipt{}, fmt.Errorf("%w: correlation ID", ErrInvalidRequest)
	}
	// A retry after a lost reply replays from the payment store alone, even when
	// Core Banking is unavailable.
	if existing, found, err := service.store.FindIdempotency(ctx, scope); err != nil {
		return Receipt{}, err
	} else if found {
		if existing.RequestHash != hash {
			return Receipt{}, ErrIdempotencyConflict
		}
		receipt, err := service.store.Receipt(ctx, existing.PaymentID, existing.CommandID)
		receipt.Replayed = true
		return receipt, err
	}
	if err := service.authorizeAccounts(ctx, instruction); err != nil {
		return Receipt{}, err
	}
	return service.store.Receive(ctx, NewReceipt{
		Scope: scope, Instruction: instruction, CorrelationID: request.CorrelationID,
		Actor: domain.Actor{Type: domain.CustomerActor, Subject: request.Principal.Subject},
	})
}

func (service *Service) authorizeAccounts(ctx context.Context, instruction domain.Instruction) error {
	debtor, err := service.accounts.Account(ctx, instruction.DebtorAccountID)
	if errors.Is(err, ErrAccountNotFound) {
		// Unknown and foreign accounts are indistinguishable to the caller.
		return fmt.Errorf("%w: debtor account access denied", ErrForbidden)
	}
	if err != nil {
		return err
	}
	if debtor.CustomerID == nil || *debtor.CustomerID != instruction.CustomerID || debtor.Kind != "CUSTOMER_DEPOSIT" {
		return fmt.Errorf("%w: debtor account access denied", ErrForbidden)
	}
	if debtor.Status != "OPEN" || debtor.Currency != instruction.Amount.Currency {
		return fmt.Errorf("%w: debtor account must be open in the payment currency", ErrInvalidRequest)
	}
	if instruction.Product != domain.BookTransfer {
		return nil
	}
	creditor, err := service.accounts.Account(ctx, *instruction.Creditor.AccountID)
	if errors.Is(err, ErrAccountNotFound) {
		return fmt.Errorf("%w: creditor account is not eligible", ErrInvalidRequest)
	}
	if err != nil {
		return err
	}
	if creditor.Kind != "CUSTOMER_DEPOSIT" || creditor.Status != "OPEN" || creditor.Currency != instruction.Amount.Currency {
		return fmt.Errorf("%w: creditor account is not eligible", ErrInvalidRequest)
	}
	return nil
}

// Payment returns the authoritative command-store record, including a pending
// dispatch, even before any projection exists.
func (service *Service) Payment(ctx context.Context, principal auth.Principal, paymentID value.ID) (PaymentRecord, error) {
	if err := paymentID.Validate(); err != nil {
		return PaymentRecord{}, fmt.Errorf("%w: payment ID must be a canonical UUIDv4", ErrInvalidRequest)
	}
	record, err := service.store.Payment(ctx, paymentID)
	if err != nil {
		return PaymentRecord{}, err
	}
	if !canRead(principal, record.Payment.CustomerID) {
		return PaymentRecord{}, ErrForbidden
	}
	return record, nil
}

func (service *Service) Command(ctx context.Context, principal auth.Principal, commandID value.ID) (Command, error) {
	if err := commandID.Validate(); err != nil {
		return Command{}, fmt.Errorf("%w: command ID must be a canonical UUIDv4", ErrInvalidRequest)
	}
	command, err := service.store.Command(ctx, commandID)
	if err != nil {
		return Command{}, err
	}
	if !canRead(principal, command.CustomerID) {
		return Command{}, ErrForbidden
	}
	return command, nil
}

func canRead(principal auth.Principal, customerID value.ID) bool {
	if principal.HasRole(CustomerRole) && principal.CanAccessCustomer(string(customerID)) {
		return true
	}
	for _, role := range OperatorReadRoles {
		if principal.HasRole(role) {
			return true
		}
	}
	return false
}
