// Package api is the payment command HTTP boundary. Types in api.gen.go are
// generated from contracts/openapi/v1/payment.json (make openapi-generate); the
// contract validates wire shape, while the domain and application layers own
// product rules, idempotency and authorization.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgconn"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
)

//go:embed payment.bundle.json
var bundle []byte

const maxBodyBytes = 64 * 1024

type PaymentService interface {
	Submit(context.Context, application.SubmitRequest) (application.Receipt, error)
	Payment(context.Context, auth.Principal, value.ID) (application.PaymentRecord, error)
	Command(context.Context, auth.Principal, value.ID) (application.Command, error)
}

// JournalRole may read the payment event journal; projection consumers use it
// to fill ordering gaps and rebuild. It grants no command access.
const JournalRole = "internal_service"

// Register mounts the payment routes behind authentication and route-level
// roles. The service still enforces customer scope on every record.
func Register(mux *http.ServeMux, verifier auth.Verifier, service PaymentService, journal application.Journal) error {
	handler, err := NewHTTPHandler(service, journal)
	if err != nil {
		return err
	}
	submitters := auth.RequireAnyRole(application.CustomerRole)
	readers := auth.RequireAnyRole(append([]string{application.CustomerRole}, application.OperatorReadRoles...)...)
	mux.Handle("POST /api/v1/payments", auth.Authenticate(verifier, submitters(handler)))
	mux.Handle("GET /api/v1/payments/{paymentId}/receipt", auth.Authenticate(verifier, readers(handler)))
	mux.Handle("GET /api/v1/commands/{commandId}", auth.Authenticate(verifier, readers(handler)))
	journalReaders := auth.RequireAnyRole(JournalRole)
	mux.Handle("GET /integrations/v1/payment-events", auth.Authenticate(verifier, journalReaders(handler)))
	mux.Handle("GET /integrations/v1/payment-events/watermark", auth.Authenticate(verifier, journalReaders(handler)))
	mux.Handle("GET /integrations/v1/payments/{paymentId}/events", auth.Authenticate(verifier, journalReaders(handler)))
	return nil
}

// NewHTTPHandler validates requests against the embedded contract bundle and
// dispatches to the generated standard-library server wrapper.
func NewHTTPHandler(service PaymentService, journal application.Journal) (http.Handler, error) {
	validate, err := requestValidator()
	if err != nil {
		return nil, err
	}
	return validate(HandlerWithOptions(&server{service: service, journal: journal}, StdHTTPServerOptions{
		ErrorHandlerFunc: func(writer http.ResponseWriter, request *http.Request, _ error) {
			platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, "request parameters do not match the payment API contract", false)
		},
	})), nil
}

func requestValidator() (func(http.Handler) http.Handler, error) {
	document, err := openapi3.NewLoader().LoadFromData(bundle)
	if err != nil {
		return nil, err
	}
	// Match routes on any host; deployment addresses are not part of validation.
	document.Servers = nil
	if err := document.Validate(context.Background()); err != nil {
		return nil, err
	}
	router, err := legacy.NewRouter(document)
	if err != nil {
		return nil, err
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			route, parameters, err := router.FindRoute(request)
			if err != nil {
				next.ServeHTTP(writer, request)
				return
			}
			if request.Body != nil {
				request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
			}
			if err := openapi3filter.ValidateRequest(request.Context(), &openapi3filter.RequestValidationInput{
				Request: request, PathParams: parameters, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			}); err != nil {
				platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, "request does not match the payment API contract", false)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}, nil
}

type server struct {
	service PaymentService
	journal application.Journal
}

const defaultJournalPage = 200

func (server *server) GetPaymentJournalWatermark(writer http.ResponseWriter, request *http.Request, _ GetPaymentJournalWatermarkParams) {
	watermark, err := server.journal.JournalWatermark(request.Context())
	if err != nil {
		writeError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, JournalWatermark{
		Position: strconv.FormatInt(watermark.Position, 10), Events: strconv.FormatInt(watermark.Events, 10),
		Payments: strconv.FormatInt(watermark.Aggregates, 10),
	})
}

func (server *server) ListPaymentJournal(writer http.ResponseWriter, request *http.Request, params ListPaymentJournalParams) {
	if params.UpToPosition < params.AfterPosition {
		writeError(writer, request, fmt.Errorf("%w: upToPosition must not be below afterPosition", application.ErrInvalidRequest))
		return
	}
	entries, err := server.journal.Journal(request.Context(), params.AfterPosition, params.UpToPosition, pageSize(params.Limit))
	writeJournal(writer, request, entries, err)
}

func (server *server) ListPaymentEvents(writer http.ResponseWriter, request *http.Request, paymentID ContractsCommonV1TypesDefsID, params ListPaymentEventsParams) {
	id := value.ID(paymentID)
	if err := id.Validate(); err != nil {
		writeError(writer, request, fmt.Errorf("%w: payment ID must be a canonical UUIDv4", application.ErrInvalidRequest))
		return
	}
	entries, err := server.journal.AggregateJournal(request.Context(), id, params.AfterSequence, pageSize(params.Limit))
	writeJournal(writer, request, entries, err)
}

func pageSize(limit *int) int32 {
	if limit == nil {
		return defaultJournalPage
	}
	return int32(*limit)
}

func writeJournal(writer http.ResponseWriter, request *http.Request, entries []application.JournalEntry, err error) {
	if err != nil {
		writeError(writer, request, err)
		return
	}
	page := JournalPage{Entries: make([]JournalEntry, len(entries))}
	for index, entry := range entries {
		var event map[string]interface{}
		encoded, err := json.Marshal(entry.Event)
		if err == nil {
			err = json.Unmarshal(encoded, &event)
		}
		if err != nil {
			writeError(writer, request, err)
			return
		}
		page.Entries[index] = JournalEntry{Position: strconv.FormatInt(entry.Position, 10), Event: event}
	}
	platformruntime.WriteJSON(writer, http.StatusOK, page)
}

func (server *server) SubmitPayment(writer http.ResponseWriter, request *http.Request, params SubmitPaymentParams) {
	var body PaymentRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, request, application.ErrInvalidRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, request, application.ErrInvalidRequest)
		return
	}
	instruction, err := toInstruction(body)
	if err != nil {
		writeError(writer, request, err)
		return
	}
	principal, _ := auth.PrincipalFromContext(request.Context())
	receipt, err := server.service.Submit(request.Context(), application.SubmitRequest{
		Principal: principal, IdempotencyKey: params.IdempotencyKey, Instruction: instruction,
		CorrelationID: platformruntime.CorrelationID(request.Context()),
	})
	if err != nil {
		writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotent-Replayed", strconv.FormatBool(receipt.Replayed))
	platformruntime.WriteJSON(writer, http.StatusAccepted, PaymentReceipt{
		PaymentId: string(receipt.Payment.ID), CommandId: string(receipt.CommandID),
		ReceiptVersion: strconv.FormatInt(receipt.Payment.Version, 10),
		LifecycleState: LifecycleState(receipt.Payment.Lifecycle), ExecutionOutcome: ExecutionOutcome(receipt.Payment.Outcome),
		Dispatch: toDispatch(receipt.Dispatch), ReceivedAt: receipt.Payment.ReceivedAt.UTC(),
	})
}

func (server *server) GetPaymentReceipt(writer http.ResponseWriter, request *http.Request, paymentID string, _ GetPaymentReceiptParams) {
	principal, _ := auth.PrincipalFromContext(request.Context())
	record, err := server.service.Payment(request.Context(), principal, value.ID(paymentID))
	if err != nil {
		writeError(writer, request, err)
		return
	}
	payment := record.Payment
	response := PaymentRecord{
		PaymentId: string(payment.ID), CustomerId: string(payment.CustomerID),
		Version:        strconv.FormatInt(payment.Version, 10),
		LifecycleState: LifecycleState(payment.Lifecycle), ExecutionOutcome: ExecutionOutcome(payment.Outcome),
		Instruction: fromInstruction(record.Instruction), Dispatch: toDispatch(record.Dispatch),
		ReceivedAt: payment.ReceivedAt.UTC(), UpdatedAt: payment.UpdatedAt.UTC(),
	}
	if payment.ExecutionWorkflowID != "" {
		owner := payment.ExecutionWorkflowID
		response.ExecutionWorkflowId = &owner
	}
	if payment.Claim != nil {
		response.Claim = &Claim{Kind: ClaimKind(payment.Claim.Kind), ClaimId: string(payment.Claim.ClaimID), ClaimedAt: payment.Claim.ClaimedAt.UTC()}
	}
	platformruntime.WriteJSON(writer, http.StatusOK, response)
}

func (server *server) GetCommand(writer http.ResponseWriter, request *http.Request, commandID string, _ GetCommandParams) {
	principal, _ := auth.PrincipalFromContext(request.Context())
	command, err := server.service.Command(request.Context(), principal, value.ID(commandID))
	if err != nil {
		writeError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, CommandReceipt{
		CommandId: string(command.CommandID), CommandType: CommandReceiptCommandType(command.CommandType),
		PaymentId: string(command.PaymentID), Status: CommandReceiptStatus(command.Status),
		CommittedVersion: strconv.FormatInt(command.CommittedVersion, 10),
		CorrelationId:    string(command.CorrelationID), RecordedAt: command.RecordedAt.UTC(),
	})
}

func toInstruction(body PaymentRequest) (domain.Instruction, error) {
	amount, err := strconv.ParseInt(body.Amount.AmountMinor, 10, 64)
	if err != nil {
		return domain.Instruction{}, application.ErrInvalidRequest
	}
	deref := func(text *string) string {
		if text == nil {
			return ""
		}
		return *text
	}
	creditor := domain.Creditor{
		Name: body.Creditor.Name, AccountNumber: deref(body.Creditor.AccountNumber),
		BankIdentifier: deref(body.Creditor.BankIdentifier), Country: deref(body.Creditor.Country),
	}
	if body.Creditor.AccountId != nil {
		accountID := value.ID(*body.Creditor.AccountId)
		creditor.AccountID = &accountID
	}
	return domain.Instruction{
		CustomerID: value.ID(body.CustomerId), Product: domain.Product(body.Product),
		DebtorAccountID: value.ID(body.DebtorAccountId),
		Amount:          value.Money{AmountMinor: amount, Currency: value.Currency(body.Amount.Currency)},
		Creditor:        creditor, Reference: deref(body.Reference),
	}, nil
}

func fromInstruction(instruction domain.Instruction) PaymentRequest {
	optional := func(text string) *string {
		if text == "" {
			return nil
		}
		return &text
	}
	creditor := Creditor{
		Name: instruction.Creditor.Name, AccountNumber: optional(instruction.Creditor.AccountNumber),
		BankIdentifier: optional(instruction.Creditor.BankIdentifier), Country: optional(instruction.Creditor.Country),
	}
	if instruction.Creditor.AccountID != nil {
		accountID := string(*instruction.Creditor.AccountID)
		creditor.AccountId = &accountID
	}
	return PaymentRequest{
		CustomerId: string(instruction.CustomerID), Product: Product(instruction.Product),
		DebtorAccountId: string(instruction.DebtorAccountID),
		Amount: PositiveMoney{
			AmountMinor: strconv.FormatInt(instruction.Amount.AmountMinor, 10),
			Currency:    PositiveMoneyCurrency(instruction.Amount.Currency),
		},
		Creditor: creditor, Reference: optional(instruction.Reference),
	}
}

func toDispatch(dispatch application.Dispatch) Dispatch {
	return Dispatch{
		WorkflowId: dispatch.WorkflowID, Status: DispatchStatus(dispatch.Status),
		Attempts: int(dispatch.Attempts), UpdatedAt: dispatch.UpdatedAt.UTC(),
	}
}

func writeError(writer http.ResponseWriter, request *http.Request, err error) {
	var pgError *pgconn.PgError
	switch {
	case errors.Is(err, application.ErrInvalidRequest), errors.Is(err, domain.ErrInvalidInstruction):
		platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, safeMessage(err), false)
	case errors.Is(err, application.ErrForbidden):
		platformruntime.WriteError(writer, request, http.StatusForbidden, contracts.Forbidden, "payment access denied", false)
	case errors.Is(err, application.ErrNotFound):
		platformruntime.WriteError(writer, request, http.StatusNotFound, contracts.NotFound, "payment record not found", false)
	case errors.Is(err, application.ErrIdempotencyConflict):
		platformruntime.WriteError(writer, request, http.StatusConflict, contracts.Conflict, "Idempotency-Key was already used with a different payload", false)
	case errors.Is(err, application.ErrDependencyUnavailable):
		platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "account authorization is temporarily unavailable; retry with the same Idempotency-Key", true)
	case errors.As(err, &pgError), errors.Is(err, context.DeadlineExceeded):
		platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "payment database unavailable; retry with the same Idempotency-Key", true)
	default:
		platformruntime.WriteError(writer, request, http.StatusInternalServerError, contracts.InternalError, "internal server error", false)
	}
}

// safeMessage keeps the most specific validation reason without internal detail.
func safeMessage(err error) string {
	message := err.Error()
	if index := strings.LastIndex(message, ": "); index >= 0 {
		message = message[index+2:]
	}
	if len(message) == 0 || len(message) > 200 {
		return "invalid payment request"
	}
	return message
}
