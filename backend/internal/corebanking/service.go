package corebanking

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gtb.local/backend/internal/corebanking/domain"
	"gtb.local/backend/internal/corebanking/storage/coredb"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
	"gtb.local/backend/internal/platform/value"
)

var (
	ErrNotFound          = errors.New("core banking record not found")
	ErrConflict          = errors.New("operation identity was reused with a different request")
	ErrInsufficientFunds = errors.New("insufficient available funds")
	ErrInvalidState      = errors.New("core banking record is not in the required state")
	ErrExpiredHold       = errors.New("hold expired before execution claim")
	ErrInvalidRequest    = errors.New("invalid Core Banking request")
)

type Service struct {
	pool  *pgxpool.Pool
	now   func() time.Time
	newID func() (value.ID, error)
}

type ReserveRequest struct {
	OperationID   value.ID    `json:"operationId"`
	AccountID     value.ID    `json:"accountId"`
	Amount        value.Money `json:"amount"`
	ExpiresAt     *time.Time  `json:"expiresAt,omitempty"`
	CorrelationID value.ID    `json:"-"`
}

type ReleaseRequest struct {
	OperationID   value.ID `json:"operationId"`
	HoldID        value.ID `json:"holdId"`
	CorrelationID value.ID `json:"-"`
}

type PostRequest struct {
	OperationID   value.ID       `json:"operationId"`
	HoldID        *value.ID      `json:"holdId,omitempty"`
	Description   string         `json:"description"`
	Entries       []domain.Entry `json:"entries"`
	CorrelationID value.ID       `json:"-"`
}

type ReverseRequest struct {
	OperationID       value.ID `json:"operationId"`
	OriginalPostingID value.ID `json:"originalPostingId"`
	Description       string   `json:"description"`
	CorrelationID     value.ID `json:"-"`
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now, newID: value.NewID}
}

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("GTB_DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Core Banking database configuration: %w", err)
	}
	config.MaxConns = 16
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	platformtelemetry.ConfigurePGX(config)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Core Banking database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Core Banking database: %w", err)
	}
	if err := platformtelemetry.RecordPGXStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("instrument Core Banking database: %w", err)
	}
	return pool, nil
}

func (service *Service) Ready(ctx context.Context) error {
	return service.pool.Ping(ctx)
}

func (service *Service) GetAccount(ctx context.Context, accountID value.ID) (domain.Account, error) {
	id, err := parseID(accountID)
	if err != nil {
		return domain.Account{}, invalidRequest(err.Error())
	}
	queries := coredb.New(service.pool)
	row, err := queries.GetAccount(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, ErrNotFound
	}
	if err != nil {
		return domain.Account{}, err
	}
	holds, err := queries.SumActiveHolds(ctx, id)
	if err != nil {
		return domain.Account{}, err
	}
	return accountFromRow(row, holds), nil
}

func (service *Service) GetOperation(ctx context.Context, operationID value.ID) (domain.Operation, error) {
	id, err := parseID(operationID)
	if err != nil {
		return domain.Operation{}, invalidRequest(err.Error())
	}
	row, err := coredb.New(service.pool).GetOperation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Operation{}, ErrNotFound
	}
	if err != nil {
		return domain.Operation{}, err
	}
	return operationFromRow(row), nil
}

func (service *Service) GetHold(ctx context.Context, holdID value.ID) (domain.Hold, error) {
	id, err := parseID(holdID)
	if err != nil {
		return domain.Hold{}, invalidRequest(err.Error())
	}
	row, err := coredb.New(service.pool).GetHold(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Hold{}, ErrNotFound
	}
	if err != nil {
		return domain.Hold{}, err
	}
	return holdFromRow(row), nil
}

func (service *Service) Reserve(ctx context.Context, request ReserveRequest) (domain.Hold, error) {
	if err := validateRequestIDs(request.OperationID, request.AccountID, request.CorrelationID); err != nil {
		return domain.Hold{}, invalidRequest(err.Error())
	}
	if err := request.Amount.Validate(); err != nil || request.Amount.AmountMinor <= 0 {
		return domain.Hold{}, invalidRequest("reservation amount must be positive and valid")
	}
	if request.ExpiresAt != nil && !request.ExpiresAt.After(service.now()) {
		return domain.Hold{}, invalidRequest("reservation expiry must be in the future")
	}
	hash, err := requestHash(struct {
		AccountID value.ID    `json:"accountId"`
		Amount    value.Money `json:"amount"`
		ExpiresAt *time.Time  `json:"expiresAt,omitempty"`
	}{request.AccountID, request.Amount, request.ExpiresAt})
	if err != nil {
		return domain.Hold{}, err
	}

	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Hold{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := coredb.New(tx)
	if err := queries.LockOperationIdentity(ctx, string(request.OperationID)); err != nil {
		return domain.Hold{}, err
	}
	if existing, found, err := findOperation(ctx, queries, request.OperationID); err != nil {
		return domain.Hold{}, err
	} else if found {
		if existing.OperationType != "RESERVE" || existing.RequestHash != hash {
			return domain.Hold{}, ErrConflict
		}
		hold, err := queries.GetHold(ctx, existing.ResourceID)
		if err != nil {
			return domain.Hold{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Hold{}, err
		}
		return holdFromRow(hold), nil
	}

	accountID, _ := parseID(request.AccountID)
	account, err := queries.LockAccount(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Hold{}, ErrNotFound
	}
	if err != nil {
		return domain.Hold{}, err
	}
	if account.Status != "OPEN" || domain.AccountKind(account.AccountKind) != domain.CustomerDeposit {
		return domain.Hold{}, ErrInvalidState
	}
	if value.Currency(account.Currency) != request.Amount.Currency {
		return domain.Hold{}, invalidRequest("reservation currency does not match account")
	}
	active, err := queries.SumActiveHolds(ctx, accountID)
	if err != nil {
		return domain.Hold{}, err
	}
	if active > account.BookedBalanceMinor || request.Amount.AmountMinor > account.BookedBalanceMinor-active {
		return domain.Hold{}, ErrInsufficientFunds
	}
	holdValueID, err := service.newID()
	if err != nil {
		return domain.Hold{}, err
	}
	holdID, _ := parseID(holdValueID)
	operationID, _ := parseID(request.OperationID)
	correlationID, _ := parseID(request.CorrelationID)
	if _, err := queries.InsertOperation(ctx, coredb.InsertOperationParams{
		OperationID: operationID, OperationType: "RESERVE", RequestHash: hash,
		ResourceType: "HOLD", ResourceID: holdID, CorrelationID: correlationID,
	}); err != nil {
		return domain.Hold{}, err
	}
	hold, err := queries.InsertHold(ctx, coredb.InsertHoldParams{
		HoldID: holdID, AccountID: accountID, ReserveOperationID: operationID,
		AmountMinor: request.Amount.AmountMinor, Currency: string(request.Amount.Currency),
		ExpiresAt: nullableTime(request.ExpiresAt),
	})
	if err != nil {
		return domain.Hold{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Hold{}, err
	}
	return holdFromRow(hold), nil
}

func (service *Service) Release(ctx context.Context, request ReleaseRequest) (domain.Hold, error) {
	if err := validateRequestIDs(request.OperationID, request.HoldID, request.CorrelationID); err != nil {
		return domain.Hold{}, invalidRequest(err.Error())
	}
	hash, _ := requestHash(struct {
		HoldID value.ID `json:"holdId"`
	}{request.HoldID})
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Hold{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := coredb.New(tx)
	if err := queries.LockOperationIdentity(ctx, string(request.OperationID)); err != nil {
		return domain.Hold{}, err
	}
	if existing, found, err := findOperation(ctx, queries, request.OperationID); err != nil {
		return domain.Hold{}, err
	} else if found {
		if existing.OperationType != "RELEASE" || existing.RequestHash != hash {
			return domain.Hold{}, ErrConflict
		}
		hold, err := queries.GetHold(ctx, existing.ResourceID)
		if err != nil {
			return domain.Hold{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Hold{}, err
		}
		return holdFromRow(hold), nil
	}
	holdID, _ := parseID(request.HoldID)
	hold, err := queries.LockHold(ctx, holdID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Hold{}, ErrNotFound
	}
	if err != nil {
		return domain.Hold{}, err
	}
	if hold.Status != string(domain.HoldActive) {
		return domain.Hold{}, ErrInvalidState
	}
	operationID, _ := parseID(request.OperationID)
	correlationID, _ := parseID(request.CorrelationID)
	if _, err := queries.InsertOperation(ctx, coredb.InsertOperationParams{
		OperationID: operationID, OperationType: "RELEASE", RequestHash: hash,
		ResourceType: "HOLD", ResourceID: holdID, CorrelationID: correlationID,
	}); err != nil {
		return domain.Hold{}, err
	}
	hold, err = queries.ReleaseHold(ctx, coredb.ReleaseHoldParams{
		HoldID: holdID, ReleasedByOperationID: pgtype.UUID{Bytes: operationID, Valid: true},
	})
	if err != nil {
		return domain.Hold{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Hold{}, err
	}
	return holdFromRow(hold), nil
}

func (service *Service) Post(ctx context.Context, request PostRequest) (domain.Posting, error) {
	return service.post(ctx, request, nil, "POST")
}

func (service *Service) Reverse(ctx context.Context, request ReverseRequest) (domain.Posting, error) {
	if err := validateRequestIDs(request.OperationID, request.OriginalPostingID, request.CorrelationID); err != nil {
		return domain.Posting{}, invalidRequest(err.Error())
	}
	if strings.TrimSpace(request.Description) == "" || len(request.Description) > 256 {
		return domain.Posting{}, invalidRequest("reversal description must contain 1 to 256 characters")
	}
	originalID, _ := parseID(request.OriginalPostingID)
	queries := coredb.New(service.pool)
	original, err := queries.GetPosting(ctx, originalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Posting{}, ErrNotFound
	}
	if err != nil {
		return domain.Posting{}, err
	}
	if original.ReversalOfPostingID.Valid {
		return domain.Posting{}, ErrInvalidState
	}
	rows, err := queries.ListPostingEntries(ctx, originalID)
	if err != nil {
		return domain.Posting{}, err
	}
	entries := make([]domain.Entry, len(rows))
	for index, row := range rows {
		side := domain.Debit
		if row.Side == string(domain.Debit) {
			side = domain.Credit
		}
		entries[index] = domain.Entry{
			AccountID: value.ID(row.AccountID.String()), Side: side,
			Amount: value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		}
	}
	return service.post(ctx, PostRequest{
		OperationID: request.OperationID, Description: request.Description,
		Entries: entries, CorrelationID: request.CorrelationID,
	}, &request.OriginalPostingID, "REVERSE")
}

func (service *Service) post(ctx context.Context, request PostRequest, reversalOf *value.ID, operationType string) (domain.Posting, error) {
	if err := validateRequestIDs(request.OperationID, request.CorrelationID); err != nil {
		return domain.Posting{}, invalidRequest(err.Error())
	}
	if request.HoldID != nil {
		if err := request.HoldID.Validate(); err != nil {
			return domain.Posting{}, invalidRequest(err.Error())
		}
	}
	if strings.TrimSpace(request.Description) == "" || len(request.Description) > 256 {
		return domain.Posting{}, invalidRequest("posting description must contain 1 to 256 characters")
	}
	if err := domain.ValidateEntries(request.Entries); err != nil {
		return domain.Posting{}, invalidRequest(err.Error())
	}
	hash, err := requestHash(struct {
		HoldID      *value.ID      `json:"holdId,omitempty"`
		Description string         `json:"description"`
		Entries     []domain.Entry `json:"entries"`
		ReversalOf  *value.ID      `json:"reversalOf,omitempty"`
	}{request.HoldID, request.Description, request.Entries, reversalOf})
	if err != nil {
		return domain.Posting{}, err
	}

	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Posting{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := coredb.New(tx)
	if err := queries.LockOperationIdentity(ctx, string(request.OperationID)); err != nil {
		return domain.Posting{}, err
	}
	if existing, found, err := findOperation(ctx, queries, request.OperationID); err != nil {
		return domain.Posting{}, err
	} else if found {
		if existing.OperationType != operationType || existing.RequestHash != hash {
			return domain.Posting{}, ErrConflict
		}
		posting, err := loadPosting(ctx, queries, existing.ResourceID)
		if err != nil {
			return domain.Posting{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Posting{}, err
		}
		return posting, nil
	}

	var lockedHold *coredb.AppHold
	if request.HoldID != nil {
		holdID, _ := parseID(*request.HoldID)
		hold, err := queries.LockHold(ctx, holdID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Posting{}, ErrNotFound
		}
		if err != nil {
			return domain.Posting{}, err
		}
		if hold.Status != string(domain.HoldActive) {
			return domain.Posting{}, ErrInvalidState
		}
		if hold.ExpiresAt.Valid && !hold.ExpiresAt.Time.After(service.now()) {
			return domain.Posting{}, ErrExpiredHold
		}
		lockedHold = &hold
	}

	accounts := make(map[value.ID]coredb.AppAccount, len(request.Entries))
	for _, accountValueID := range domain.SortedAccountIDs(request.Entries) {
		accountID, _ := parseID(accountValueID)
		row, err := queries.LockAccount(ctx, accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Posting{}, ErrNotFound
		}
		if err != nil {
			return domain.Posting{}, err
		}
		if row.Status != "OPEN" {
			return domain.Posting{}, ErrInvalidState
		}
		accounts[accountValueID] = row
	}
	if lockedHold != nil {
		if err := validateHoldConsumption(*lockedHold, request.Entries); err != nil {
			return domain.Posting{}, err
		}
	}

	balances := make(map[value.ID]int64, len(accounts))
	for id, account := range accounts {
		balances[id] = account.BookedBalanceMinor
	}
	for _, entry := range request.Entries {
		account := accounts[entry.AccountID]
		if account.Currency != string(entry.Amount.Currency) {
			return domain.Posting{}, invalidRequest("entry currency does not match account")
		}
		next, err := domain.BalanceAfter(domain.AccountKind(account.AccountKind), balances[entry.AccountID], entry.Side, entry.Amount.AmountMinor)
		if err != nil {
			return domain.Posting{}, ErrInsufficientFunds
		}
		balances[entry.AccountID] = next
	}

	postingValueID, err := service.newID()
	if err != nil {
		return domain.Posting{}, err
	}
	eventValueID, err := service.newID()
	if err != nil {
		return domain.Posting{}, err
	}
	postingID, _ := parseID(postingValueID)
	eventID, _ := parseID(eventValueID)
	operationID, _ := parseID(request.OperationID)
	correlationID, _ := parseID(request.CorrelationID)
	if _, err := queries.InsertOperation(ctx, coredb.InsertOperationParams{
		OperationID: operationID, OperationType: operationType, RequestHash: hash,
		ResourceType: "POSTING", ResourceID: postingID, CorrelationID: correlationID,
	}); err != nil {
		return domain.Posting{}, err
	}
	if lockedHold != nil {
		if _, err := queries.ConsumeHold(ctx, coredb.ConsumeHoldParams{
			HoldID:                lockedHold.HoldID,
			ConsumedByOperationID: pgtype.UUID{Bytes: operationID, Valid: true},
		}); err != nil {
			return domain.Posting{}, err
		}
	}
	for id, next := range balances {
		account := accounts[id]
		if domain.AccountKind(account.AccountKind) == domain.CustomerDeposit {
			active, err := queries.SumActiveHolds(ctx, account.AccountID)
			if err != nil {
				return domain.Posting{}, err
			}
			if active > next {
				return domain.Posting{}, ErrInsufficientFunds
			}
		}
		if _, err := queries.UpdateAccountBalance(ctx, coredb.UpdateAccountBalanceParams{
			AccountID: account.AccountID, BookedBalanceMinor: next,
		}); err != nil {
			return domain.Posting{}, err
		}
	}
	journal, err := queries.InsertPosting(ctx, coredb.InsertPostingParams{
		PostingID: postingID, OperationID: operationID, Description: request.Description,
		ReversalOfPostingID: nullableUUID(reversalOf),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Posting{}, ErrConflict
		}
		return domain.Posting{}, err
	}
	for index, entry := range request.Entries {
		accountID, _ := parseID(entry.AccountID)
		if err := queries.InsertPostingEntry(ctx, coredb.InsertPostingEntryParams{
			PostingID: postingID, LineNumber: int16(index + 1), AccountID: accountID,
			Side: string(entry.Side), AmountMinor: entry.Amount.AmountMinor,
			Currency: string(entry.Amount.Currency),
		}); err != nil {
			return domain.Posting{}, err
		}
	}
	occurredAt := service.now().UTC()
	payload, _ := json.Marshal(map[string]any{
		"operationId": request.OperationID, "postingId": postingValueID,
		"reversalOfPostingId": reversalOf, "entries": request.Entries,
	})
	if _, err := queries.InsertPostingEvent(ctx, coredb.InsertPostingEventParams{
		EventID: eventID, AggregateID: postingID,
		OccurredAt:    pgtype.Timestamptz{Time: occurredAt, Valid: true},
		CorrelationID: correlationID, CausationID: operationID, Payload: payload,
	}); err != nil {
		return domain.Posting{}, err
	}
	if err := queries.InsertPostingOutbox(ctx, coredb.InsertPostingOutboxParams{
		EventID: eventID, OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
	}); err != nil {
		return domain.Posting{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Posting{}, err
	}
	return domain.Posting{
		PostingID: postingValueID, OperationID: request.OperationID,
		Description: request.Description, ReversalOfPostingID: reversalOf,
		Entries: append([]domain.Entry(nil), request.Entries...), EventID: eventValueID,
		CommittedAt: journal.CommittedAt.Time.UTC(),
	}, nil
}

func (service *Service) Statement(ctx context.Context, accountID value.ID, from, to *time.Time, limit int32) ([]domain.StatementEntry, error) {
	id, err := parseID(accountID)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}
	if limit < 1 || limit > 500 {
		return nil, invalidRequest("statement limit must be between 1 and 500")
	}
	if _, err := coredb.New(service.pool).GetAccount(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := coredb.New(service.pool).ListStatement(ctx, coredb.ListStatementParams{
		AccountID: id, Column2: nullableTime(from), Column3: nullableTime(to), Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	entries := make([]domain.StatementEntry, len(rows))
	for index, row := range rows {
		entries[index] = domain.StatementEntry{
			PostingID: value.ID(row.PostingID.String()), OperationID: value.ID(row.OperationID.String()),
			Description: row.Description, ReversalOfPostingID: valueIDPointer(row.ReversalOfPostingID),
			CommittedAt: row.CommittedAt.Time.UTC(), Side: domain.Side(row.Side),
			Amount: value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		}
	}
	return entries, nil
}

func findOperation(ctx context.Context, queries *coredb.Queries, operationID value.ID) (coredb.AppOperation, bool, error) {
	id, _ := parseID(operationID)
	row, err := queries.GetOperation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return coredb.AppOperation{}, false, nil
	}
	return row, err == nil, err
}

func loadPosting(ctx context.Context, queries *coredb.Queries, postingID uuid.UUID) (domain.Posting, error) {
	journal, err := queries.GetPosting(ctx, postingID)
	if err != nil {
		return domain.Posting{}, err
	}
	rows, err := queries.ListPostingEntries(ctx, postingID)
	if err != nil {
		return domain.Posting{}, err
	}
	event, err := queries.GetPostingEvent(ctx, postingID)
	if err != nil {
		return domain.Posting{}, err
	}
	entries := make([]domain.Entry, len(rows))
	for index, row := range rows {
		entries[index] = domain.Entry{
			AccountID: value.ID(row.AccountID.String()), Side: domain.Side(row.Side),
			Amount: value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		}
	}
	return domain.Posting{
		PostingID: value.ID(journal.PostingID.String()), OperationID: value.ID(journal.OperationID.String()),
		Description: journal.Description, ReversalOfPostingID: valueIDPointer(journal.ReversalOfPostingID),
		Entries: entries, EventID: value.ID(event.EventID.String()), CommittedAt: journal.CommittedAt.Time.UTC(),
	}, nil
}

func validateHoldConsumption(hold coredb.AppHold, entries []domain.Entry) error {
	var debit int64
	for _, entry := range entries {
		if entry.AccountID != value.ID(hold.AccountID.String()) {
			continue
		}
		if entry.Side != domain.Debit || entry.Amount.Currency != value.Currency(hold.Currency) {
			return invalidRequest("held account must have only the reserved debit in its consuming posting")
		}
		if entry.Side == domain.Debit {
			if debit > value.MaxMinorUnits-entry.Amount.AmountMinor {
				return domain.ErrBounds
			}
			debit += entry.Amount.AmountMinor
		}
	}
	if debit != hold.AmountMinor {
		return invalidRequest("hold must be consumed by an equal debit on its account and currency")
	}
	return nil
}

func validateRequestIDs(ids ...value.ID) error {
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func requestHash(payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func parseID(id value.ID) (uuid.UUID, error) {
	if err := id.Validate(); err != nil {
		return uuid.Nil, err
	}
	return uuid.FromString(string(id))
}

func nullableUUID(id *value.ID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	parsed, _ := parseID(*id)
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

func nullableTime(timestamp *time.Time) pgtype.Timestamptz {
	if timestamp == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: timestamp.UTC(), Valid: true}
}

func valueIDPointer(id pgtype.UUID) *value.ID {
	if !id.Valid {
		return nil
	}
	valueID := value.ID(uuid.UUID(id.Bytes).String())
	return &valueID
}

func accountFromRow(row coredb.AppAccount, activeHolds int64) domain.Account {
	available := row.BookedBalanceMinor
	if domain.AccountKind(row.AccountKind) == domain.CustomerDeposit {
		available -= activeHolds
	}
	return domain.Account{
		AccountID: value.ID(row.AccountID.String()), ExternalReference: row.ExternalReference,
		CustomerID: valueIDPointer(row.CustomerID), Kind: domain.AccountKind(row.AccountKind),
		Currency: value.Currency(row.Currency), BookedMinor: row.BookedBalanceMinor,
		AvailableMinor: available, Status: row.Status,
		CreatedAt: row.CreatedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
}

func holdFromRow(row coredb.AppHold) domain.Hold {
	var expiry *time.Time
	if row.ExpiresAt.Valid {
		value := row.ExpiresAt.Time.UTC()
		expiry = &value
	}
	return domain.Hold{
		HoldID: value.ID(row.HoldID.String()), AccountID: value.ID(row.AccountID.String()),
		ReserveOperationID: value.ID(row.ReserveOperationID.String()),
		Amount:             value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		Status:             domain.HoldStatus(row.Status), ExpiresAt: expiry,
		ConsumedByOperationID: valueIDPointer(row.ConsumedByOperationID),
		ReleasedByOperationID: valueIDPointer(row.ReleasedByOperationID),
		CreatedAt:             row.CreatedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
}

func operationFromRow(row coredb.AppOperation) domain.Operation {
	return domain.Operation{
		OperationID: value.ID(row.OperationID.String()), OperationType: row.OperationType,
		ResourceType: row.ResourceType, ResourceID: value.ID(row.ResourceID.String()),
		CorrelationID: value.ID(row.CorrelationID.String()), CommittedAt: row.CommittedAt.Time.UTC(),
	}
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}

func invalidRequest(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, message)
}
