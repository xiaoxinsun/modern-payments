// Package query serves the payment read models. It reads only the projections
// database through the read-only query_reader login, enforces customer and
// operator scope, and reports freshness. A durably received payment that is not
// yet projected is answered from the payment command store's receipt.
package query

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gtb.local/backend/internal/platform/auth"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

var (
	ErrInvalidRequest        = errors.New("invalid query")
	ErrForbidden             = errors.New("payment access denied")
	ErrNotFound              = errors.New("payment not found")
	ErrDependencyUnavailable = errors.New("payment command store unavailable")
)

const CustomerRole = "customer"

// OperatorReadRoles may read any payment, matching the command-store receipt.
var OperatorReadRoles = []string{"ops_viewer", "investigator", "approver", "auditor"}

// StatusReadRoles may read projection delivery health.
var StatusReadRoles = append(slices.Clone(OperatorReadRoles), "platform_admin")

type Freshness string

const (
	Current Freshness = "CURRENT"
	Lagging Freshness = "LAGGING"
	Stale   Freshness = "STALE"
)

type Meta struct {
	AsOf         time.Time
	Generation   int32
	Freshness    Freshness
	ConsumerLag  int64
	BrokerSeenAt *time.Time
	LastEventAt  *time.Time
}

type ViewStatus string

const (
	Projected ViewStatus = "PROJECTED"
	Behind    ViewStatus = "BEHIND"
	Pending   ViewStatus = "PENDING"
)

// Summary is one payment's projected state. Customer, product and amount are
// absent only in a PENDING view built from a receipt that lacks them.
type Summary struct {
	PaymentID        value.ID
	CustomerID       value.ID
	Product          string
	Amount           *value.Money
	LifecycleState   string
	ExecutionOutcome string
	Version          int64
	ReceivedAt       time.Time
	LastEventAt      *time.Time
	ProjectedAt      *time.Time
}

type View struct {
	Summary
	Status ViewStatus
	Meta   Meta
}

type Page struct {
	Items      []Summary
	NextCursor string
	Meta       Meta
}

type TimelineEntry struct {
	EventID          value.ID
	EventSequence    int64
	EventType        string
	AggregateVersion int64
	LifecycleState   string
	ExecutionOutcome string
	OccurredAt       time.Time
	CorrelationID    value.ID
	CausationID      value.ID
}

type Timeline struct {
	PaymentID value.ID
	Status    ViewStatus
	Entries   []TimelineEntry
	Meta      Meta
}

// Receipt is the command store's authoritative receipt, fetched with the
// caller's own token so the owner applies its authorization.
type Receipt struct {
	PaymentID        value.ID
	CustomerID       value.ID
	Product          string
	Amount           value.Money
	LifecycleState   string
	ExecutionOutcome string
	Version          int64
	ReceivedAt       time.Time
}

// Receipts returns ErrNotFound, ErrForbidden or ErrDependencyUnavailable.
type Receipts interface {
	Receipt(ctx context.Context, authorization string, paymentID value.ID) (Receipt, error)
}

type Service struct {
	pool       *pgxpool.Pool
	receipts   Receipts
	staleAfter time.Duration
	now        func() time.Time
}

func NewService(pool *pgxpool.Pool, receipts Receipts, staleAfter time.Duration) *Service {
	return &Service{pool: pool, receipts: receipts, staleAfter: staleAfter, now: time.Now}
}

func (service *Service) Ready(ctx context.Context) error {
	return service.pool.Ping(ctx)
}

// Payment returns the projected view, or the pending receipt when the payment
// is not projected yet. minVersion 0 means no version requirement.
func (service *Service) Payment(ctx context.Context, principal auth.Principal, authorization string, paymentID value.ID, minVersion int64) (View, error) {
	if err := paymentID.Validate(); err != nil {
		return View{}, fmt.Errorf("%w: payment ID must be a canonical UUIDv4", ErrInvalidRequest)
	}
	meta, err := service.meta(ctx)
	if err != nil {
		return View{}, err
	}
	row, err := projectiondb.New(service.pool).ActivePayment(ctx, mustUUID(paymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		receipt, err := service.receipts.Receipt(ctx, authorization, paymentID)
		if err != nil {
			return View{}, err
		}
		amount := receipt.Amount
		return View{Status: Pending, Meta: meta, Summary: Summary{
			PaymentID: receipt.PaymentID, CustomerID: receipt.CustomerID, Product: receipt.Product, Amount: &amount,
			LifecycleState: receipt.LifecycleState, ExecutionOutcome: receipt.ExecutionOutcome,
			Version: receipt.Version, ReceivedAt: receipt.ReceivedAt,
		}}, nil
	}
	if err != nil {
		return View{}, err
	}
	summary := summaryFromRow(row)
	if !canRead(principal, summary.CustomerID) {
		return View{}, ErrForbidden
	}
	status := Projected
	if minVersion > summary.Version {
		status = Behind
	}
	return View{Summary: summary, Status: status, Meta: meta}, nil
}

func (service *Service) Timeline(ctx context.Context, principal auth.Principal, authorization string, paymentID value.ID) (Timeline, error) {
	view, err := service.Payment(ctx, principal, authorization, paymentID, 0)
	if err != nil {
		return Timeline{}, err
	}
	timeline := Timeline{PaymentID: paymentID, Status: view.Status, Meta: view.Meta, Entries: []TimelineEntry{}}
	if view.Status == Pending {
		return timeline, nil
	}
	rows, err := projectiondb.New(service.pool).ActiveTimeline(ctx, mustUUID(paymentID))
	if err != nil {
		return Timeline{}, err
	}
	for _, row := range rows {
		timeline.Entries = append(timeline.Entries, TimelineEntry{
			EventID: value.ID(row.EventID.String()), EventSequence: row.EventSequence, EventType: row.EventType,
			AggregateVersion: row.AggregateVersion, LifecycleState: row.LifecycleState, ExecutionOutcome: row.ExecutionOutcome,
			OccurredAt: row.OccurredAt.Time.UTC(), CorrelationID: value.ID(row.CorrelationID.String()),
			CausationID: value.ID(row.CausationID.String()),
		})
	}
	return timeline, nil
}

type ListRequest struct {
	CustomerID     *value.ID
	LifecycleState *string
	Limit          int
	Cursor         string
}

type cursor struct {
	ReceivedAt time.Time `json:"r"`
	PaymentID  value.ID  `json:"p"`
	Scope      string    `json:"s"`
}

// List pages projected payments newest first. Customers must name a customer
// in their token scope; operators may search everything.
func (service *Service) List(ctx context.Context, principal auth.Principal, request ListRequest) (Page, error) {
	params := projectiondb.ListActivePaymentsParams{LifecycleState: request.LifecycleState, PageSize: int32(request.Limit) + 1}
	if request.CustomerID != nil {
		if err := request.CustomerID.Validate(); err != nil {
			return Page{}, fmt.Errorf("%w: customerId must be a canonical UUIDv4", ErrInvalidRequest)
		}
		params.CustomerIds = []uuid.UUID{mustUUID(*request.CustomerID)}
	}
	switch {
	case isOperator(principal):
	case principal.HasRole(CustomerRole):
		if request.CustomerID == nil {
			return Page{}, fmt.Errorf("%w: customerId is required for customer searches", ErrInvalidRequest)
		}
		if !principal.CanAccessCustomer(string(*request.CustomerID)) {
			return Page{}, ErrForbidden
		}
	default:
		return Page{}, ErrForbidden
	}
	scope := cursorScope(principal, request)
	if request.Cursor != "" {
		position, err := decodeCursor(request.Cursor, scope)
		if err != nil {
			return Page{}, err
		}
		params.BeforeReceivedAt = pgtype.Timestamptz{Time: position.ReceivedAt, Valid: true}
		params.BeforePaymentID = pgtype.UUID{Bytes: mustUUID(position.PaymentID), Valid: true}
	}
	meta, err := service.meta(ctx)
	if err != nil {
		return Page{}, err
	}
	rows, err := projectiondb.New(service.pool).ListActivePayments(ctx, params)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Summary{}, Meta: meta}
	for index, row := range rows {
		if index == request.Limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeCursor(cursor{ReceivedAt: last.ReceivedAt, PaymentID: last.PaymentID, Scope: scope})
			break
		}
		page.Items = append(page.Items, summaryFromRow(row))
	}
	return page, nil
}

// Status reports projection delivery health for operators.
type Status struct {
	AsOf        time.Time
	Generations []projectiondb.ListGenerationsRow
	Consumers   []ConsumerStatus
	Buffered    Backlog
	Repairs     Backlog
}

type ConsumerStatus struct {
	Consumer     string
	Freshness    Freshness
	BrokerSeenAt *time.Time
	LastEventAt  *time.Time
	Partitions   []projectiondb.AppConsumerOffset
}

type Backlog struct {
	Count    int64
	OldestAt *time.Time
}

func (service *Service) Status(ctx context.Context) (Status, error) {
	queries := projectiondb.New(service.pool)
	generations, err := queries.ListGenerations(ctx)
	if err != nil {
		return Status{}, err
	}
	offsets, err := queries.ConsumerLag(ctx)
	if err != nil {
		return Status{}, err
	}
	statuses, err := queries.ConsumerStatuses(ctx)
	if err != nil {
		return Status{}, err
	}
	backlog, err := queries.DeliveryBacklog(ctx)
	if err != nil {
		return Status{}, err
	}
	now := service.now().UTC()
	status := Status{
		AsOf: now, Generations: generations,
		Buffered: Backlog{Count: backlog.Buffered, OldestAt: optionalTime(backlog.OldestBufferedAt)},
		Repairs:  Backlog{Count: backlog.OpenRepairs, OldestAt: optionalTime(backlog.OldestRepairAt)},
	}
	for _, row := range statuses {
		consumer := ConsumerStatus{
			Consumer: row.Consumer, BrokerSeenAt: optionalTime(row.BrokerSeenAt), LastEventAt: optionalTime(row.LastEventAt),
			Partitions: []projectiondb.AppConsumerOffset{},
		}
		var lag int64
		for _, offset := range offsets {
			if offset.Consumer == row.Consumer {
				consumer.Partitions = append(consumer.Partitions, offset)
				lag += max(offset.HighWatermark-offset.NextOffset, 0)
			}
		}
		consumer.Freshness = service.freshness(now, consumer.BrokerSeenAt, lag)
		status.Consumers = append(status.Consumers, consumer)
	}
	return status, nil
}

// meta describes the active generation and the live consumer's freshness.
func (service *Service) meta(ctx context.Context) (Meta, error) {
	queries := projectiondb.New(service.pool)
	generations, err := queries.ListGenerations(ctx)
	if err != nil {
		return Meta{}, err
	}
	now := service.now().UTC()
	meta := Meta{AsOf: now}
	for _, generation := range generations {
		if generation.Status == "ACTIVE" {
			meta.Generation = generation.Generation
		}
	}
	if meta.Generation == 0 {
		return Meta{}, errors.New("no active projection generation")
	}
	statuses, err := queries.ConsumerStatuses(ctx)
	if err != nil {
		return Meta{}, err
	}
	for _, row := range statuses {
		if row.Consumer == projections.PaymentConsumer {
			meta.BrokerSeenAt, meta.LastEventAt = optionalTime(row.BrokerSeenAt), optionalTime(row.LastEventAt)
		}
	}
	offsets, err := queries.ConsumerLag(ctx)
	if err != nil {
		return Meta{}, err
	}
	for _, offset := range offsets {
		if offset.Consumer == projections.PaymentConsumer {
			meta.ConsumerLag += max(offset.HighWatermark-offset.NextOffset, 0)
		}
	}
	meta.Freshness = service.freshness(now, meta.BrokerSeenAt, meta.ConsumerLag)
	return meta, nil
}

func (service *Service) freshness(now time.Time, brokerSeenAt *time.Time, lag int64) Freshness {
	switch {
	case brokerSeenAt == nil || now.Sub(*brokerSeenAt) > service.staleAfter:
		return Stale
	case lag > 0:
		return Lagging
	default:
		return Current
	}
}

func canRead(principal auth.Principal, customerID value.ID) bool {
	return isOperator(principal) || (principal.HasRole(CustomerRole) && principal.CanAccessCustomer(string(customerID)))
}

func isOperator(principal auth.Principal) bool {
	for _, role := range OperatorReadRoles {
		if principal.HasRole(role) {
			return true
		}
	}
	return false
}

// cursorScope binds a cursor to its caller and filters, so it cannot be
// replayed under another identity or a different search.
func cursorScope(principal auth.Principal, request ListRequest) string {
	parts := []string{principal.Subject}
	if request.CustomerID != nil {
		parts = append(parts, string(*request.CustomerID))
	}
	parts = append(parts, "|")
	if request.LifecycleState != nil {
		parts = append(parts, *request.LifecycleState)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func encodeCursor(position cursor) string {
	encoded, _ := json.Marshal(position)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeCursor(raw, scope string) (cursor, error) {
	var position cursor
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		err = json.Unmarshal(decoded, &position)
	}
	if err != nil || position.Scope != scope || position.PaymentID.Validate() != nil || position.ReceivedAt.IsZero() {
		return cursor{}, fmt.Errorf("%w: cursor is invalid for this search", ErrInvalidRequest)
	}
	return position, nil
}

func summaryFromRow(row projectiondb.AppPaymentSummary) Summary {
	lastEventAt, projectedAt := row.LastEventAt.Time.UTC(), row.ProjectedAt.Time.UTC()
	return Summary{
		PaymentID: value.ID(row.PaymentID.String()), CustomerID: value.ID(row.CustomerID.String()), Product: row.Product,
		Amount:         &value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		LifecycleState: row.LifecycleState, ExecutionOutcome: row.ExecutionOutcome, Version: row.ProjectedVersion,
		ReceivedAt: row.ReceivedAt.Time.UTC(), LastEventAt: &lastEventAt, ProjectedAt: &projectedAt,
	}
}

func optionalTime(at pgtype.Timestamptz) *time.Time {
	if !at.Valid {
		return nil
	}
	utc := at.Time.UTC()
	return &utc
}

func mustUUID(id value.ID) uuid.UUID {
	parsed, err := uuid.FromString(string(id))
	if err != nil {
		panic(fmt.Sprintf("unvalidated identifier reached query storage: %v", err))
	}
	return parsed
}
