// Package api is the payment query HTTP boundary. Types in api.gen.go are
// generated from contracts/openapi/v1/query.json (make openapi-generate); the
// query service owns scoping, freshness and the pending-receipt fallback.
package api

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgconn"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/query"
)

//go:embed query.bundle.json
var bundle []byte

type QueryService interface {
	Payment(ctx context.Context, principal auth.Principal, authorization string, paymentID value.ID, minVersion int64) (query.View, error)
	Timeline(ctx context.Context, principal auth.Principal, authorization string, paymentID value.ID) (query.Timeline, error)
	List(ctx context.Context, principal auth.Principal, request query.ListRequest) (query.Page, error)
	Status(ctx context.Context) (query.Status, error)
}

// Register mounts the query routes behind authentication and route-level
// roles; the service applies customer scope to every record.
func Register(mux *http.ServeMux, verifier auth.Verifier, service QueryService) error {
	handler, err := NewHTTPHandler(service)
	if err != nil {
		return err
	}
	readers := auth.RequireAnyRole(append([]string{query.CustomerRole}, query.OperatorReadRoles...)...)
	mux.Handle("GET /api/v1/payments", auth.Authenticate(verifier, readers(handler)))
	mux.Handle("GET /api/v1/payments/{paymentId}", auth.Authenticate(verifier, readers(handler)))
	mux.Handle("GET /api/v1/payments/{paymentId}/timeline", auth.Authenticate(verifier, readers(handler)))
	mux.Handle("GET /api/v1/ops/projections", auth.Authenticate(verifier, auth.RequireAnyRole(query.StatusReadRoles...)(handler)))
	return nil
}

func NewHTTPHandler(service QueryService) (http.Handler, error) {
	document, err := openapi3.NewLoader().LoadFromData(bundle)
	if err != nil {
		return nil, err
	}
	document.Servers = nil
	if err := document.Validate(context.Background()); err != nil {
		return nil, err
	}
	router, err := legacy.NewRouter(document)
	if err != nil {
		return nil, err
	}
	generated := HandlerWithOptions(&server{service: service}, StdHTTPServerOptions{
		ErrorHandlerFunc: func(writer http.ResponseWriter, request *http.Request, _ error) {
			platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, "request parameters do not match the query API contract", false)
		},
	})
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if route, parameters, err := router.FindRoute(request); err == nil {
			if err := openapi3filter.ValidateRequest(request.Context(), &openapi3filter.RequestValidationInput{
				Request: request, PathParams: parameters, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			}); err != nil {
				platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, "request does not match the query API contract", false)
				return
			}
		}
		generated.ServeHTTP(writer, request)
	}), nil
}

type server struct {
	service QueryService
}

func (server *server) GetPayment(writer http.ResponseWriter, request *http.Request, paymentID ContractsCommonV1TypesDefsID, params GetPaymentParams) {
	principal, _ := auth.PrincipalFromContext(request.Context())
	var minVersion int64
	if params.MinVersion != nil {
		minVersion = *params.MinVersion
	}
	view, err := server.service.Payment(request.Context(), principal, request.Header.Get("Authorization"), value.ID(paymentID), minVersion)
	if err != nil {
		writeError(writer, request, err)
		return
	}
	summary := view.Summary
	body := PaymentView{
		PaymentId: string(summary.PaymentID), Status: PaymentViewStatus(view.Status),
		LifecycleState: PaymentLifecycleState(summary.LifecycleState), ExecutionOutcome: PaymentExecutionOutcome(summary.ExecutionOutcome),
		Version: strconv.FormatInt(summary.Version, 10), ReceivedAt: summary.ReceivedAt,
		LastEventAt: summary.LastEventAt, ProjectedAt: summary.ProjectedAt, Projection: meta(view.Meta),
	}
	if summary.CustomerID != "" {
		customerID := string(summary.CustomerID)
		body.CustomerId = &customerID
	}
	if summary.Product != "" {
		product := PaymentProduct(summary.Product)
		body.Product = &product
	}
	if summary.Amount != nil {
		body.Amount = money(*summary.Amount)
	}
	platformruntime.WriteJSON(writer, http.StatusOK, body)
}

func (server *server) GetPaymentTimeline(writer http.ResponseWriter, request *http.Request, paymentID ContractsCommonV1TypesDefsID, _ GetPaymentTimelineParams) {
	principal, _ := auth.PrincipalFromContext(request.Context())
	timeline, err := server.service.Timeline(request.Context(), principal, request.Header.Get("Authorization"), value.ID(paymentID))
	if err != nil {
		writeError(writer, request, err)
		return
	}
	body := PaymentTimeline{
		PaymentId: string(timeline.PaymentID), Status: PaymentTimelineStatus(timeline.Status),
		Entries: make([]TimelineEntry, len(timeline.Entries)), Projection: meta(timeline.Meta),
	}
	for index, entry := range timeline.Entries {
		body.Entries[index] = TimelineEntry{
			EventId: string(entry.EventID), EventSequence: strconv.FormatInt(entry.EventSequence, 10), EventType: entry.EventType,
			AggregateVersion: strconv.FormatInt(entry.AggregateVersion, 10), LifecycleState: PaymentLifecycleState(entry.LifecycleState),
			ExecutionOutcome: PaymentExecutionOutcome(entry.ExecutionOutcome), OccurredAt: entry.OccurredAt,
			CorrelationId: string(entry.CorrelationID), CausationId: string(entry.CausationID),
		}
	}
	platformruntime.WriteJSON(writer, http.StatusOK, body)
}

func (server *server) ListPayments(writer http.ResponseWriter, request *http.Request, params ListPaymentsParams) {
	principal, _ := auth.PrincipalFromContext(request.Context())
	list := query.ListRequest{Limit: 50}
	if params.Limit != nil {
		list.Limit = *params.Limit
	}
	if params.Cursor != nil {
		list.Cursor = *params.Cursor
	}
	if params.CustomerId != nil {
		customerID := value.ID(*params.CustomerId)
		list.CustomerID = &customerID
	}
	if params.LifecycleState != nil {
		state := string(*params.LifecycleState)
		list.LifecycleState = &state
	}
	page, err := server.service.List(request.Context(), principal, list)
	if err != nil {
		writeError(writer, request, err)
		return
	}
	body := PaymentPage{Items: make([]PaymentSummary, len(page.Items)), Projection: meta(page.Meta)}
	if page.NextCursor != "" {
		body.NextCursor = &page.NextCursor
	}
	for index, summary := range page.Items {
		body.Items[index] = PaymentSummary{
			PaymentId: string(summary.PaymentID), CustomerId: string(summary.CustomerID), Product: PaymentProduct(summary.Product),
			Amount: *money(*summary.Amount), LifecycleState: PaymentLifecycleState(summary.LifecycleState),
			ExecutionOutcome: PaymentExecutionOutcome(summary.ExecutionOutcome), Version: strconv.FormatInt(summary.Version, 10),
			ReceivedAt: summary.ReceivedAt, LastEventAt: *summary.LastEventAt,
		}
	}
	platformruntime.WriteJSON(writer, http.StatusOK, body)
}

func (server *server) GetProjectionStatus(writer http.ResponseWriter, request *http.Request, _ GetProjectionStatusParams) {
	status, err := server.service.Status(request.Context())
	if err != nil {
		writeError(writer, request, err)
		return
	}
	body := ProjectionStatus{
		AsOf: status.AsOf, Generations: make([]GenerationStatus, len(status.Generations)), Consumers: []ConsumerStatus{},
		BufferedEvents: backlog(status.Buffered), OpenRepairs: backlog(status.Repairs),
	}
	for index, generation := range status.Generations {
		body.Generations[index] = GenerationStatus{
			Generation: int(generation.Generation), Status: GenerationStatusStatus(generation.Status),
			CreatedAt: generation.CreatedAt.Time.UTC(), ActivatedAt: timeOrNil(generation.ActivatedAt.Time, generation.ActivatedAt.Valid),
			FinishedAt: timeOrNil(generation.FinishedAt.Time, generation.FinishedAt.Valid),
		}
		if generation.JournalWatermark != nil {
			watermark := strconv.FormatInt(*generation.JournalWatermark, 10)
			body.Generations[index].JournalWatermark = &watermark
		}
	}
	for _, consumer := range status.Consumers {
		entry := ConsumerStatus{
			Consumer: consumer.Consumer, Freshness: Freshness(consumer.Freshness), BrokerSeenAt: consumer.BrokerSeenAt,
			LastEventAt: consumer.LastEventAt, Partitions: make([]PartitionLag, len(consumer.Partitions)),
		}
		for index, partition := range consumer.Partitions {
			entry.Partitions[index] = PartitionLag{
				Topic: partition.Topic, Partition: int(partition.Partition), NextOffset: strconv.FormatInt(partition.NextOffset, 10),
				HighWatermark: strconv.FormatInt(partition.HighWatermark, 10),
				Lag:           strconv.FormatInt(max(partition.HighWatermark-partition.NextOffset, 0), 10),
				UpdatedAt:     partition.UpdatedAt.Time.UTC(),
			}
		}
		body.Consumers = append(body.Consumers, entry)
	}
	platformruntime.WriteJSON(writer, http.StatusOK, body)
}

func meta(source query.Meta) ProjectionMeta {
	return ProjectionMeta{
		AsOf: source.AsOf, Generation: int(source.Generation), Freshness: Freshness(source.Freshness),
		ConsumerLag: strconv.FormatInt(source.ConsumerLag, 10), BrokerSeenAt: source.BrokerSeenAt, LastEventAt: source.LastEventAt,
	}
}

func money(amount value.Money) *ContractsCommonV1TypesDefsMoney {
	return &ContractsCommonV1TypesDefsMoney{AmountMinor: strconv.FormatInt(amount.AmountMinor, 10), Currency: string(amount.Currency)}
}

func backlog(source query.Backlog) Backlog {
	return Backlog{Count: strconv.FormatInt(source.Count, 10), OldestAt: source.OldestAt}
}

func timeOrNil(at time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func writeError(writer http.ResponseWriter, request *http.Request, err error) {
	var pgError *pgconn.PgError
	switch {
	case errors.Is(err, query.ErrInvalidRequest):
		platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, "invalid payment query", false)
	case errors.Is(err, query.ErrForbidden):
		platformruntime.WriteError(writer, request, http.StatusForbidden, contracts.Forbidden, "payment access denied", false)
	case errors.Is(err, query.ErrNotFound):
		platformruntime.WriteError(writer, request, http.StatusNotFound, contracts.NotFound, "payment not found", false)
	case errors.Is(err, query.ErrDependencyUnavailable):
		platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "payment is not projected yet and the command store is unavailable; retry", true)
	case errors.As(err, &pgError), errors.Is(err, context.DeadlineExceeded):
		platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "query database unavailable; retry", true)
	default:
		platformruntime.WriteError(writer, request, http.StatusInternalServerError, contracts.InternalError, "internal server error", false)
	}
}
