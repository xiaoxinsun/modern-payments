package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/payment/storage/paymentdb"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
	"gtb.local/backend/internal/platform/value"
)

const (
	PaymentReceivedEvent = "PaymentReceived"
	submitPaymentCommand = "SUBMIT_PAYMENT"
)

type Store struct {
	pool  *pgxpool.Pool
	now   func() time.Time
	newID func() (value.ID, error)
	// failpoint lets integration tests abort a transaction after a named step.
	failpoint func(step string) error
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now, newID: value.NewID, failpoint: func(string) error { return nil }}
}

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("GTB_DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse payment database configuration: %w", err)
	}
	config.MaxConns = 16
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	platformtelemetry.ConfigurePGX(config)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open payment database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping payment database: %w", err)
	}
	if err := platformtelemetry.RecordPGXStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("instrument payment database: %w", err)
	}
	return pool, nil
}

func (store *Store) Ready(ctx context.Context) error {
	return store.pool.Ping(ctx)
}

func (store *Store) FindIdempotency(ctx context.Context, scope domain.IdempotencyScope) (application.IdempotencyRecord, bool, error) {
	return findIdempotency(ctx, paymentdb.New(store.pool), scope)
}

func findIdempotency(ctx context.Context, queries *paymentdb.Queries, scope domain.IdempotencyScope) (application.IdempotencyRecord, bool, error) {
	row, err := queries.GetIdempotencyRecord(ctx, paymentdb.GetIdempotencyRecordParams{
		Channel: string(scope.Channel), CustomerID: mustUUID(scope.CustomerID), IdempotencyKey: scope.Key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return application.IdempotencyRecord{}, false, err
	}
	return application.IdempotencyRecord{
		RequestHash: row.RequestHash, PaymentID: valueID(row.PaymentID), CommandID: valueID(row.CommandID),
	}, true, nil
}

// Receive commits one durable receipt. A scope-level advisory lock serializes
// concurrent duplicates so the loser replays or conflicts instead of failing.
func (store *Store) Receive(ctx context.Context, receipt application.NewReceipt) (application.Receipt, error) {
	if err := receipt.Scope.Validate(); err != nil {
		return application.Receipt{}, fmt.Errorf("%w: %w", application.ErrInvalidRequest, err)
	}
	ids, err := store.ids(5)
	if err != nil {
		return application.Receipt{}, err
	}
	paymentID, commandID, eventID, auditID, dispatchID := ids[0], ids[1], ids[2], ids[3], ids[4]
	now := store.now().UTC()
	payment, change, err := domain.NewPayment(paymentID, receipt.Instruction, receipt.Actor, commandID, now)
	if err != nil {
		return application.Receipt{}, fmt.Errorf("%w: %w", application.ErrInvalidRequest, err)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return application.Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := paymentdb.New(tx)
	scope := receipt.Scope
	if err := queries.LockIdempotencyScope(ctx, string(scope.Channel)+"|"+string(scope.CustomerID)+"|"+scope.Key); err != nil {
		return application.Receipt{}, err
	}
	if existing, found, err := findIdempotency(ctx, queries, scope); err != nil {
		return application.Receipt{}, err
	} else if found {
		if existing.RequestHash != payment.InstructionHash {
			return application.Receipt{}, application.ErrIdempotencyConflict
		}
		replayed, err := loadReceipt(ctx, queries, existing.PaymentID, existing.CommandID)
		if err != nil {
			return application.Receipt{}, err
		}
		replayed.Replayed = true
		return replayed, tx.Commit(ctx)
	}

	steps := []struct {
		name string
		run  func() error
	}{
		{"payment", func() error { return insertPayment(ctx, queries, payment) }},
		{"instruction", func() error { return insertInstruction(ctx, queries, payment, receipt.Instruction) }},
		{"version", func() error { return insertChange(ctx, queries, change) }},
		{"command", func() error {
			return queries.InsertCommand(ctx, paymentdb.InsertCommandParams{
				CommandID: mustUUID(commandID), CommandType: submitPaymentCommand, PaymentID: mustUUID(paymentID),
				CustomerID: mustUUID(payment.CustomerID), ActorType: string(receipt.Actor.Type),
				ActorSubject: receipt.Actor.Subject, Status: "COMMITTED", CommittedVersion: payment.Version,
				CorrelationID: mustUUID(receipt.CorrelationID), RecordedAt: timestamp(now),
			})
		}},
		{"idempotency", func() error {
			return queries.InsertIdempotencyRecord(ctx, paymentdb.InsertIdempotencyRecordParams{
				Channel: string(scope.Channel), CustomerID: mustUUID(scope.CustomerID), IdempotencyKey: scope.Key,
				RequestHash: payment.InstructionHash, PaymentID: mustUUID(paymentID), CommandID: mustUUID(commandID),
				RecordedAt: timestamp(now),
			})
		}},
		{"audit", func() error {
			return insertAudit(ctx, queries, auditID, change, "RECEIVE_PAYMENT", receipt.CorrelationID, map[string]any{
				"idempotencyChannel": scope.Channel, "product": payment.Product,
			})
		}},
		{"event", func() error {
			return insertEvent(ctx, queries, eventID, PaymentReceivedEvent, change, 1, receipt.CorrelationID, commandID, map[string]any{
				"paymentId": paymentID, "customerId": payment.CustomerID, "product": payment.Product,
				"amount": receipt.Instruction.Amount, "lifecycleState": payment.Lifecycle,
				"executionOutcome": payment.Outcome,
			})
		}},
		{"dispatch", func() error {
			return queries.InsertWorkflowDispatch(ctx, paymentdb.InsertWorkflowDispatchParams{
				DispatchID: mustUUID(dispatchID), PaymentID: mustUUID(paymentID),
				WorkflowID: domain.PaymentWorkflowID(paymentID), NextAttemptAt: timestamp(now),
			})
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			return application.Receipt{}, fmt.Errorf("receive %s: %w", step.name, err)
		}
		if err := store.failpoint(step.name); err != nil {
			return application.Receipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return application.Receipt{}, err
	}
	return application.Receipt{
		Payment: payment, CommandID: commandID,
		Dispatch: application.Dispatch{WorkflowID: domain.PaymentWorkflowID(paymentID), Status: "PENDING", UpdatedAt: now},
	}, nil
}

func (store *Store) Receipt(ctx context.Context, paymentID, commandID value.ID) (application.Receipt, error) {
	return loadReceipt(ctx, paymentdb.New(store.pool), paymentID, commandID)
}

func loadReceipt(ctx context.Context, queries *paymentdb.Queries, paymentID, commandID value.ID) (application.Receipt, error) {
	row, err := queries.GetPayment(ctx, mustUUID(paymentID))
	if err != nil {
		return application.Receipt{}, notFound(err)
	}
	dispatch, err := queries.GetWorkflowDispatch(ctx, mustUUID(paymentID))
	if err != nil {
		return application.Receipt{}, notFound(err)
	}
	return application.Receipt{Payment: paymentFromRow(row), CommandID: commandID, Dispatch: dispatchFromRow(dispatch)}, nil
}

func (store *Store) Payment(ctx context.Context, paymentID value.ID) (application.PaymentRecord, error) {
	queries := paymentdb.New(store.pool)
	id := mustUUID(paymentID)
	row, err := queries.GetPayment(ctx, id)
	if err != nil {
		return application.PaymentRecord{}, notFound(err)
	}
	instruction, err := queries.GetInstruction(ctx, id)
	if err != nil {
		return application.PaymentRecord{}, notFound(err)
	}
	dispatch, err := queries.GetWorkflowDispatch(ctx, id)
	if err != nil {
		return application.PaymentRecord{}, notFound(err)
	}
	return application.PaymentRecord{
		Payment: paymentFromRow(row), Instruction: instructionFromRow(instruction), Dispatch: dispatchFromRow(dispatch),
	}, nil
}

func (store *Store) Command(ctx context.Context, commandID value.ID) (application.Command, error) {
	row, err := paymentdb.New(store.pool).GetCommand(ctx, mustUUID(commandID))
	if err != nil {
		return application.Command{}, notFound(err)
	}
	return application.Command{
		CommandID: valueID(row.CommandID), CommandType: row.CommandType, PaymentID: valueID(row.PaymentID),
		CustomerID: valueID(row.CustomerID), Status: row.Status, CommittedVersion: row.CommittedVersion,
		CorrelationID: valueID(row.CorrelationID), RecordedAt: row.RecordedAt.Time.UTC(),
	}, nil
}

// NewEvent is a domain event emitted by a change; Apply assigns identity,
// ordered sequence and the resulting aggregate version.
type NewEvent struct {
	Type    string
	Payload map[string]any
}

// Mutation computes the next aggregate from the locked current state. A nil
// change with no error is an idempotent replay and commits nothing.
type Mutation func(domain.Payment) (domain.Payment, *domain.Change, error)

type ApplyRequest struct {
	PaymentID     value.ID
	Action        string
	CorrelationID value.ID
	Mutate        Mutation
	Events        []NewEvent
}

// Apply is the versioned change primitive used by transitions, submission and
// cancellation claims and execution-owner assignment. The domain checks the
// caller's expected version against the locked row; the conditional update
// rejects any concurrent writer that bypassed the lock.
func (store *Store) Apply(ctx context.Context, request ApplyRequest) (domain.Payment, error) {
	if err := request.CorrelationID.Validate(); err != nil {
		return domain.Payment{}, fmt.Errorf("%w: correlation ID", application.ErrInvalidRequest)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Payment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := paymentdb.New(tx)
	row, err := queries.LockPayment(ctx, mustUUID(request.PaymentID))
	if err != nil {
		return domain.Payment{}, notFound(err)
	}
	current := paymentFromRow(row)
	next, change, err := request.Mutate(current)
	if err != nil {
		return domain.Payment{}, err
	}
	if change == nil {
		return current, tx.Commit(ctx)
	}
	ids, err := store.ids(1 + len(request.Events))
	if err != nil {
		return domain.Payment{}, err
	}
	sequence := row.LastEventSequence
	var claimKind *string
	var claimID pgtype.UUID
	var claimedAt pgtype.Timestamptz
	if next.Claim != nil {
		kind := string(next.Claim.Kind)
		claimKind, claimID, claimedAt = &kind, nullableUUID(&next.Claim.ClaimID), timestamp(next.Claim.ClaimedAt)
	}
	var owner *string
	if next.ExecutionWorkflowID != "" {
		owner = &next.ExecutionWorkflowID
	}
	if err := insertChange(ctx, queries, *change); err != nil {
		return domain.Payment{}, err
	}
	for index, event := range request.Events {
		sequence++
		causation := request.CorrelationID
		if change.CommandID != nil {
			causation = *change.CommandID
		}
		if err := insertEvent(ctx, queries, ids[1+index], event.Type, *change, sequence, request.CorrelationID, causation, stampedPayload(event.Payload, next)); err != nil {
			return domain.Payment{}, err
		}
	}
	updated, err := queries.UpdatePayment(ctx, paymentdb.UpdatePaymentParams{
		LifecycleState: string(next.Lifecycle), ExecutionOutcome: string(next.Outcome),
		NextVersion: next.Version, LastEventSequence: sequence, ExecutionWorkflowID: owner,
		ClaimKind: claimKind, ClaimID: claimID, ClaimedAt: claimedAt, UpdatedAt: timestamp(next.UpdatedAt),
		PaymentID: mustUUID(request.PaymentID), ExpectedVersion: current.Version,
	})
	if err != nil {
		return domain.Payment{}, err
	}
	if updated != 1 {
		return domain.Payment{}, domain.ErrVersionConflict
	}
	if err := insertAudit(ctx, queries, ids[0], *change, request.Action, request.CorrelationID, map[string]any{
		"changeKind": change.Kind, "policyVersion": change.PolicyVersion,
	}); err != nil {
		return domain.Payment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Payment{}, err
	}
	return next, nil
}

// stampedPayload adds the resulting payment state to an event payload. Every
// payment event carries paymentId, lifecycleState and executionOutcome, so
// projections never infer state from event names (ADR 0010).
func stampedPayload(payload map[string]any, next domain.Payment) map[string]any {
	stamped := make(map[string]any, len(payload)+3)
	for key, value := range payload {
		stamped[key] = value
	}
	stamped["paymentId"] = next.ID
	stamped["lifecycleState"] = next.Lifecycle
	stamped["executionOutcome"] = next.Outcome
	return stamped
}

// Changes returns the append-only version history in order.
func (store *Store) Changes(ctx context.Context, paymentID value.ID) ([]domain.Change, error) {
	rows, err := paymentdb.New(store.pool).ListPaymentVersions(ctx, mustUUID(paymentID))
	if err != nil {
		return nil, err
	}
	changes := make([]domain.Change, len(rows))
	for index, row := range rows {
		var evidence []domain.EvidenceRef
		if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
			return nil, err
		}
		from := domain.LifecycleState("")
		if row.FromState != nil {
			from = domain.LifecycleState(*row.FromState)
		}
		changes[index] = domain.Change{
			PaymentID: valueID(row.PaymentID), Version: row.Version, Kind: domain.ChangeKind(row.ChangeKind),
			FromState: from, ToState: domain.LifecycleState(row.ToState),
			FromOutcome: domain.ExecutionOutcome(row.FromOutcome), ToOutcome: domain.ExecutionOutcome(row.ToOutcome),
			Actor:         domain.Actor{Type: domain.ActorType(row.ActorType), Subject: row.ActorSubject},
			PolicyVersion: row.PolicyVersion, Reason: row.Reason, Evidence: evidence,
			CommandID: valueIDPointer(row.CommandID), OccurredAt: row.OccurredAt.Time.UTC(),
		}
	}
	return changes, nil
}

func insertPayment(ctx context.Context, queries *paymentdb.Queries, payment domain.Payment) error {
	return queries.InsertPayment(ctx, paymentdb.InsertPaymentParams{
		PaymentID: mustUUID(payment.ID), CustomerID: mustUUID(payment.CustomerID), Product: string(payment.Product),
		InstructionHash: payment.InstructionHash, LifecycleState: string(payment.Lifecycle),
		ExecutionOutcome: string(payment.Outcome), Version: payment.Version, LastEventSequence: 1,
		ReceivedAt: timestamp(payment.ReceivedAt), UpdatedAt: timestamp(payment.UpdatedAt),
	})
}

func insertInstruction(ctx context.Context, queries *paymentdb.Queries, payment domain.Payment, instruction domain.Instruction) error {
	creditor := instruction.Creditor
	return queries.InsertInstruction(ctx, paymentdb.InsertInstructionParams{
		PaymentID: mustUUID(payment.ID), InstructionHash: payment.InstructionHash,
		CustomerID: mustUUID(instruction.CustomerID), Product: string(instruction.Product),
		DebtorAccountID: mustUUID(instruction.DebtorAccountID), AmountMinor: instruction.Amount.AmountMinor,
		Currency: string(instruction.Amount.Currency), CreditorName: creditor.Name,
		CreditorAccountID: nullableUUID(creditor.AccountID), CreditorAccountNumber: optional(creditor.AccountNumber),
		CreditorBankIdentifier: optional(creditor.BankIdentifier), CreditorCountry: optional(creditor.Country),
		Reference: optional(instruction.Reference),
	})
}

func insertChange(ctx context.Context, queries *paymentdb.Queries, change domain.Change) error {
	evidence, err := json.Marshal(append([]domain.EvidenceRef{}, change.Evidence...))
	if err != nil {
		return err
	}
	return queries.InsertPaymentVersion(ctx, paymentdb.InsertPaymentVersionParams{
		PaymentID: mustUUID(change.PaymentID), Version: change.Version, ChangeKind: string(change.Kind),
		FromState: optional(string(change.FromState)), ToState: string(change.ToState),
		FromOutcome: string(change.FromOutcome), ToOutcome: string(change.ToOutcome),
		ActorType: string(change.Actor.Type), ActorSubject: change.Actor.Subject,
		PolicyVersion: change.PolicyVersion, Reason: change.Reason, Evidence: evidence,
		CommandID: nullableUUID(change.CommandID), OccurredAt: timestamp(change.OccurredAt),
	})
}

func insertAudit(ctx context.Context, queries *paymentdb.Queries, auditID value.ID, change domain.Change, action string, correlationID value.ID, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return queries.InsertAuditEntry(ctx, paymentdb.InsertAuditEntryParams{
		AuditID: mustUUID(auditID), OccurredAt: timestamp(change.OccurredAt), ActorType: string(change.Actor.Type),
		ActorSubject: change.Actor.Subject, Action: action, AggregateID: mustUUID(change.PaymentID),
		AggregateVersion: change.Version, CommandID: nullableUUID(change.CommandID),
		CorrelationID: mustUUID(correlationID), Details: encoded,
	})
}

func insertEvent(ctx context.Context, queries *paymentdb.Queries, eventID value.ID, eventType string, change domain.Change, sequence int64, correlationID, causationID value.ID, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := queries.InsertDomainEvent(ctx, paymentdb.InsertDomainEventParams{
		EventID: mustUUID(eventID), EventType: eventType, AggregateID: mustUUID(change.PaymentID),
		AggregateVersion: change.Version, EventSequence: sequence, OccurredAt: timestamp(change.OccurredAt),
		CorrelationID: mustUUID(correlationID), CausationID: mustUUID(causationID), Payload: encoded,
	}); err != nil {
		return err
	}
	return queries.InsertOutbox(ctx, paymentdb.InsertOutboxParams{EventID: mustUUID(eventID), OccurredAt: timestamp(change.OccurredAt)})
}

func paymentFromRow(row paymentdb.AppPayment) domain.Payment {
	payment := domain.Payment{
		ID: valueID(row.PaymentID), CustomerID: valueID(row.CustomerID), Product: domain.Product(row.Product),
		InstructionHash: row.InstructionHash, Lifecycle: domain.LifecycleState(row.LifecycleState),
		Outcome: domain.ExecutionOutcome(row.ExecutionOutcome), Version: row.Version,
		ReceivedAt: row.ReceivedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
	if row.ExecutionWorkflowID != nil {
		payment.ExecutionWorkflowID = *row.ExecutionWorkflowID
	}
	if row.ClaimKind != nil && row.ClaimID.Valid {
		payment.Claim = &domain.Claim{
			Kind: domain.ClaimKind(*row.ClaimKind), ClaimID: *valueIDPointer(row.ClaimID), ClaimedAt: row.ClaimedAt.Time.UTC(),
		}
	}
	return payment
}

func instructionFromRow(row paymentdb.AppPaymentInstruction) domain.Instruction {
	deref := func(text *string) string {
		if text == nil {
			return ""
		}
		return *text
	}
	return domain.Instruction{
		CustomerID: valueID(row.CustomerID), Product: domain.Product(row.Product),
		DebtorAccountID: valueID(row.DebtorAccountID),
		Amount:          value.Money{AmountMinor: row.AmountMinor, Currency: value.Currency(row.Currency)},
		Creditor: domain.Creditor{
			Name: row.CreditorName, AccountID: valueIDPointer(row.CreditorAccountID),
			AccountNumber: deref(row.CreditorAccountNumber), BankIdentifier: deref(row.CreditorBankIdentifier),
			Country: deref(row.CreditorCountry),
		},
		Reference: deref(row.Reference),
	}
}

func dispatchFromRow(row paymentdb.AppWorkflowDispatch) application.Dispatch {
	return application.Dispatch{
		WorkflowID: row.WorkflowID, Status: row.Status, Attempts: row.Attempts, UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
}

func (store *Store) ids(count int) ([]value.ID, error) {
	ids := make([]value.ID, count)
	for index := range ids {
		id, err := store.newID()
		if err != nil {
			return nil, err
		}
		ids[index] = id
	}
	return ids, nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}
	return err
}

// mustUUID converts an already validated ID; invalid input yields uuid.Nil,
// which every foreign key and lookup rejects.
func mustUUID(id value.ID) uuid.UUID {
	parsed, err := uuid.FromString(string(id))
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func valueID(id uuid.UUID) value.ID {
	return value.ID(id.String())
}

func nullableUUID(id *value.ID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: mustUUID(*id), Valid: true}
}

func valueIDPointer(id pgtype.UUID) *value.ID {
	if !id.Valid {
		return nil
	}
	converted := valueID(uuid.UUID(id.Bytes))
	return &converted
}

func timestamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at.UTC(), Valid: true}
}

func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}
