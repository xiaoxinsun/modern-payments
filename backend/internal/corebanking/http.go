package corebanking

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/corebanking/domain"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
)

type BankingService interface {
	GetAccount(context.Context, value.ID) (domain.Account, error)
	GetOperation(context.Context, value.ID) (domain.Operation, error)
	GetHold(context.Context, value.ID) (domain.Hold, error)
	Reserve(context.Context, ReserveRequest) (domain.Hold, error)
	Release(context.Context, ReleaseRequest) (domain.Hold, error)
	Post(context.Context, PostRequest) (domain.Posting, error)
	Reverse(context.Context, ReverseRequest) (domain.Posting, error)
	Statement(context.Context, value.ID, *time.Time, *time.Time, int32) ([]domain.StatementEntry, error)
}

type HTTPHandler struct {
	service BankingService
}

func NewHTTPHandler(service BankingService) http.Handler {
	handler := &HTTPHandler{service: service}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /integrations/v1/core-banking/accounts/{accountId}", handler.account)
	mux.HandleFunc("GET /integrations/v1/core-banking/accounts/{accountId}/statement", handler.statement)
	mux.HandleFunc("GET /integrations/v1/core-banking/operations/{operationId}", handler.operation)
	mux.HandleFunc("GET /integrations/v1/core-banking/holds/{holdId}", handler.hold)
	mux.HandleFunc("POST /integrations/v1/core-banking/holds", handler.reserve)
	mux.HandleFunc("POST /integrations/v1/core-banking/holds/{holdId}/releases", handler.release)
	mux.HandleFunc("POST /integrations/v1/core-banking/postings", handler.post)
	mux.HandleFunc("POST /integrations/v1/core-banking/reversals", handler.reverse)
	return mux
}

func (handler *HTTPHandler) account(writer http.ResponseWriter, request *http.Request) {
	account, err := handler.service.GetAccount(request.Context(), value.ID(request.PathValue("accountId")))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, account)
}

func (handler *HTTPHandler) operation(writer http.ResponseWriter, request *http.Request) {
	operation, err := handler.service.GetOperation(request.Context(), value.ID(request.PathValue("operationId")))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, operation)
}

func (handler *HTTPHandler) hold(writer http.ResponseWriter, request *http.Request) {
	hold, err := handler.service.GetHold(request.Context(), value.ID(request.PathValue("holdId")))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, hold)
}

func (handler *HTTPHandler) reserve(writer http.ResponseWriter, request *http.Request) {
	var command ReserveRequest
	if err := decodeCommand(writer, request, &command); err != nil {
		writeBankingError(writer, request, err)
		return
	}
	command.CorrelationID = platformruntime.CorrelationID(request.Context())
	hold, err := handler.service.Reserve(request.Context(), command)
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, hold)
}

func (handler *HTTPHandler) release(writer http.ResponseWriter, request *http.Request) {
	var command struct {
		OperationID value.ID `json:"operationId"`
	}
	if err := decodeCommand(writer, request, &command); err != nil {
		writeBankingError(writer, request, err)
		return
	}
	hold, err := handler.service.Release(request.Context(), ReleaseRequest{
		OperationID: command.OperationID, HoldID: value.ID(request.PathValue("holdId")),
		CorrelationID: platformruntime.CorrelationID(request.Context()),
	})
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, hold)
}

func (handler *HTTPHandler) post(writer http.ResponseWriter, request *http.Request) {
	var command PostRequest
	if err := decodeCommand(writer, request, &command); err != nil {
		writeBankingError(writer, request, err)
		return
	}
	command.CorrelationID = platformruntime.CorrelationID(request.Context())
	posting, err := handler.service.Post(request.Context(), command)
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, posting)
}

func (handler *HTTPHandler) reverse(writer http.ResponseWriter, request *http.Request) {
	var command ReverseRequest
	if err := decodeCommand(writer, request, &command); err != nil {
		writeBankingError(writer, request, err)
		return
	}
	command.CorrelationID = platformruntime.CorrelationID(request.Context())
	posting, err := handler.service.Reverse(request.Context(), command)
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, posting)
}

func (handler *HTTPHandler) statement(writer http.ResponseWriter, request *http.Request) {
	limit := int64(100)
	var err error
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.ParseInt(raw, 10, 32)
		if err != nil {
			writeBankingError(writer, request, invalidRequest("statement limit must be an integer"))
			return
		}
	}
	from, err := optionalTime(request.URL.Query().Get("from"))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	to, err := optionalTime(request.URL.Query().Get("to"))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	entries, err := handler.service.Statement(request.Context(), value.ID(request.PathValue("accountId")), from, to, int32(limit))
	if err != nil {
		writeBankingError(writer, request, err)
		return
	}
	platformruntime.WriteJSON(writer, http.StatusOK, map[string]any{
		"accountId": request.PathValue("accountId"), "entries": entries,
	})
}

func decodeCommand(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 64*1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalidRequest("request body must be one valid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidRequest("request body must contain only one JSON object")
	}
	return nil
}

func optionalTime(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, invalidRequest("statement timestamps must use RFC3339")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func writeBankingError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		platformruntime.WriteError(writer, request, http.StatusNotFound, contracts.NotFound, "Core Banking record not found", false)
	case errors.Is(err, ErrConflict):
		platformruntime.WriteError(writer, request, http.StatusConflict, contracts.Conflict, "operation identity conflicts with its original request", false)
	case errors.Is(err, ErrInsufficientFunds):
		platformruntime.WriteError(writer, request, http.StatusConflict, contracts.Conflict, "insufficient available funds", false)
	case errors.Is(err, ErrInvalidState), errors.Is(err, ErrExpiredHold):
		platformruntime.WriteError(writer, request, http.StatusConflict, contracts.Conflict, "Core Banking record is not executable", false)
	case errors.Is(err, ErrInvalidRequest):
		message := strings.TrimPrefix(err.Error(), ErrInvalidRequest.Error()+": ")
		platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, message, false)
	default:
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) {
			platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "Core Banking database unavailable", true)
			return
		}
		platformruntime.WriteError(writer, request, http.StatusInternalServerError, contracts.InternalError, "internal server error", false)
	}
}
