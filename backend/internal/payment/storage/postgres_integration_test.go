package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/domain"
	"gtb.local/backend/internal/platform/value"
)

var (
	mayaAccount   = value.ID("10000000-0000-4000-8000-000000000001")
	danielAccount = value.ID("10000000-0000-4000-8000-000000000002")
	worker        = domain.Actor{Type: domain.SystemActor, Subject: "payment-worker"}
)

// Each test uses a fresh random customer ID, so assertions are isolated from
// other data in the shared local database.
func openTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("GTB_PAYMENT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("GTB_PAYMENT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool), pool
}

func newID(t *testing.T) value.ID {
	t.Helper()
	id, err := value.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newReceipt(t *testing.T, customerID value.ID, key string, amount int64) application.NewReceipt {
	t.Helper()
	creditor := danielAccount
	return application.NewReceipt{
		Scope: domain.IdempotencyScope{Channel: domain.CustomerAPI, CustomerID: customerID, Key: key},
		Instruction: domain.Instruction{
			CustomerID: customerID, Product: domain.BookTransfer, DebtorAccountID: mayaAccount,
			Amount:   value.Money{AmountMinor: amount, Currency: value.SGD},
			Creditor: domain.Creditor{Name: "Daniel", AccountID: &creditor},
		},
		Actor:         domain.Actor{Type: domain.CustomerActor, Subject: "subject-" + string(customerID)},
		CorrelationID: newID(t),
	}
}

// rowCounts counts every receipt table for one customer's payments.
func rowCounts(t *testing.T, pool *pgxpool.Pool, customerID value.ID) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for name, query := range map[string]string{
		"payments":     `SELECT count(*) FROM app.payments WHERE customer_id = $1`,
		"instructions": `SELECT count(*) FROM app.payment_instructions WHERE customer_id = $1`,
		"versions":     `SELECT count(*) FROM app.payment_versions v JOIN app.payments p USING (payment_id) WHERE p.customer_id = $1`,
		"commands":     `SELECT count(*) FROM app.commands WHERE customer_id = $1`,
		"idempotency":  `SELECT count(*) FROM app.idempotency_records WHERE customer_id = $1`,
		"audit":        `SELECT count(*) FROM app.audit_entries a JOIN app.payments p ON p.payment_id = a.aggregate_id WHERE p.customer_id = $1`,
		"events":       `SELECT count(*) FROM app.domain_events e JOIN app.payments p ON p.payment_id = e.aggregate_id WHERE p.customer_id = $1`,
		"outbox":       `SELECT count(*) FROM app.outbox o JOIN app.domain_events e USING (event_id) JOIN app.payments p ON p.payment_id = e.aggregate_id WHERE p.customer_id = $1`,
		"dispatch":     `SELECT count(*) FROM app.workflow_dispatch d JOIN app.payments p USING (payment_id) WHERE p.customer_id = $1 AND d.status = 'PENDING'`,
	} {
		var count int
		if err := pool.QueryRow(context.Background(), query, string(customerID)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[name] = count
	}
	return counts
}

func expectCounts(t *testing.T, got map[string]int, want int) {
	t.Helper()
	for name, count := range got {
		if count != want {
			t.Errorf("%s rows: got %d, want %d", name, count, want)
		}
	}
}

func TestPostgresReceiptIsAtomicAndReplayable(t *testing.T) {
	store, pool := openTestStore(t)
	ctx := context.Background()
	customer := newID(t)
	request := newReceipt(t, customer, "order-1", 10000)

	first, err := store.Receive(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Payment.Lifecycle != domain.Received || first.Payment.Version != 1 ||
		first.Dispatch.Status != "PENDING" || first.Dispatch.WorkflowID != domain.PaymentWorkflowID(first.Payment.ID) {
		t.Fatalf("unexpected receipt: %+v", first)
	}
	expectCounts(t, rowCounts(t, pool, customer), 1)

	request.CorrelationID = newID(t)
	second, err := store.Receive(ctx, request)
	if err != nil || !second.Replayed || second.Payment.ID != first.Payment.ID || second.CommandID != first.CommandID {
		t.Fatalf("replay: %+v %v", second, err)
	}
	conflicting := newReceipt(t, customer, "order-1", 10001)
	if _, err := store.Receive(ctx, conflicting); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("conflicting reuse: %v", err)
	}
	expectCounts(t, rowCounts(t, pool, customer), 1)

	record, err := store.Payment(ctx, first.Payment.ID)
	if err != nil || record.Instruction.Amount.AmountMinor != 10000 || record.Instruction.Creditor.AccountID == nil {
		t.Fatalf("payment record: %+v %v", record, err)
	}
	command, err := store.Command(ctx, first.CommandID)
	if err != nil || command.PaymentID != first.Payment.ID || command.CommittedVersion != 1 || command.CustomerID != customer {
		t.Fatalf("command: %+v %v", command, err)
	}
	var eventType, source string
	var sequence, version int64
	if err := pool.QueryRow(ctx, `SELECT event_type, source, event_sequence, aggregate_version FROM app.domain_events WHERE aggregate_id = $1`,
		string(first.Payment.ID)).Scan(&eventType, &source, &sequence, &version); err != nil {
		t.Fatal(err)
	}
	if eventType != PaymentReceivedEvent || source != "payment-api" || sequence != 1 || version != 1 {
		t.Fatalf("event: %s %s %d %d", eventType, source, sequence, version)
	}
	var payload string
	if err := pool.QueryRow(ctx, `SELECT payload::text FROM app.domain_events WHERE aggregate_id = $1`, string(first.Payment.ID)).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	validatePayload(t, "events/v1/payment-received.schema.json", payload)
}

func validatePayload(t *testing.T, schemaPath, payload string) {
	t.Helper()
	const root = "https://gtb.local/contracts/"
	compiler := jsonschema.NewCompiler()
	for _, path := range []string{"common/v1/types.schema.json", schemaPath} {
		data, err := os.ReadFile("../../../../contracts/" + path)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(root+path, document); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(root + schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("stored payload %s violates %s: %v", payload, schemaPath, err)
	}
}

func TestPostgresReceiptSurvivesReconnect(t *testing.T) {
	store, pool := openTestStore(t)
	ctx := context.Background()
	receipt, err := store.Receive(ctx, newReceipt(t, newID(t), "restart-1", 500))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	reopened, _ := openTestStore(t)
	record, err := reopened.Payment(ctx, receipt.Payment.ID)
	if err != nil || record.Dispatch.Status != "PENDING" || record.Payment.Lifecycle != domain.Received {
		t.Fatalf("after reconnect: %+v %v", record, err)
	}
}

func TestPostgresInjectedFailureLeavesNoPartialReceipt(t *testing.T) {
	store, pool := openTestStore(t)
	for _, step := range []string{"payment", "instruction", "version", "command", "idempotency", "audit", "event", "dispatch"} {
		t.Run(step, func(t *testing.T) {
			customer := newID(t)
			injected := errors.New("injected failure after " + step)
			store.failpoint = func(current string) error {
				if current == step {
					return injected
				}
				return nil
			}
			if _, err := store.Receive(context.Background(), newReceipt(t, customer, "fail-1", 100)); !errors.Is(err, injected) {
				t.Fatalf("got %v", err)
			}
			expectCounts(t, rowCounts(t, pool, customer), 0)
			store.failpoint = func(string) error { return nil }
			if _, err := store.Receive(context.Background(), newReceipt(t, customer, "fail-1", 100)); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
			expectCounts(t, rowCounts(t, pool, customer), 1)
		})
	}
}

func TestPostgresConcurrentDuplicatesCreateOneReceipt(t *testing.T) {
	store, pool := openTestStore(t)
	customer := newID(t)
	request := newReceipt(t, customer, "concurrent-1", 2500)
	const workers = 16
	results := make(chan application.Receipt, workers)
	failures := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			receipt, err := store.Receive(context.Background(), request)
			if err != nil {
				failures <- err
				return
			}
			results <- receipt
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	ids := map[value.ID]bool{}
	fresh := 0
	for receipt := range results {
		ids[receipt.Payment.ID] = true
		if !receipt.Replayed {
			fresh++
		}
	}
	if len(ids) != 1 || fresh != 1 {
		t.Fatalf("got %d payment IDs and %d fresh receipts", len(ids), fresh)
	}
	expectCounts(t, rowCounts(t, pool, customer), 1)
}

func transition(to domain.LifecycleState) Mutation {
	return func(payment domain.Payment) (domain.Payment, *domain.Change, error) {
		next, change, err := payment.Transition(domain.TransitionRequest{
			ExpectedVersion: payment.Version, To: to, Actor: worker, PolicyVersion: "test/v1",
			Reason: "integration test", At: time.Now(),
		})
		return next, &change, err
	}
}

func TestPostgresVersionConflictsAndClaimRace(t *testing.T) {
	store, pool := openTestStore(t)
	ctx := context.Background()
	receipt, err := store.Receive(ctx, newReceipt(t, newID(t), "claims-1", 700))
	if err != nil {
		t.Fatal(err)
	}
	id := receipt.Payment.ID
	apply := func(action string, mutate Mutation, events ...NewEvent) (domain.Payment, error) {
		return store.Apply(ctx, ApplyRequest{PaymentID: id, Action: action, CorrelationID: newID(t), Mutate: mutate, Events: events})
	}
	if _, err := apply("VALIDATE", transition(domain.Validating)); err != nil {
		t.Fatal(err)
	}
	stale := func(payment domain.Payment) (domain.Payment, *domain.Change, error) {
		next, change, err := payment.Transition(domain.TransitionRequest{
			ExpectedVersion: 1, To: domain.Accepted, Actor: worker, PolicyVersion: "test/v1", Reason: "stale", At: time.Now(),
		})
		return next, &change, err
	}
	if _, err := apply("ACCEPT", stale); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale expected version: %v", err)
	}
	accepted, err := apply("ACCEPT", transition(domain.Accepted), NewEvent{Type: "PaymentAccepted", Payload: map[string]any{"paymentId": id}})
	if err != nil || accepted.Version != 3 {
		t.Fatalf("accept: %+v %v", accepted, err)
	}

	// Submission and cancellation race from the same observed version.
	submitID, cancelID := newID(t), newID(t)
	observed := accepted.Version
	claims := []Mutation{
		func(payment domain.Payment) (domain.Payment, *domain.Change, error) {
			return payment.ClaimSubmission(observed, submitID, worker, time.Now())
		},
		func(payment domain.Payment) (domain.Payment, *domain.Change, error) {
			return payment.ClaimCancellation(observed, cancelID, domain.Actor{Type: domain.CustomerActor, Subject: "customer"}, time.Now())
		},
	}
	outcomes := make([]error, len(claims))
	var wait sync.WaitGroup
	for index, claim := range claims {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, outcomes[index] = apply("CLAIM", claim)
		}()
	}
	wait.Wait()
	winners := 0
	for _, err := range outcomes {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrClaimConflict), errors.Is(err, domain.ErrVersionConflict):
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winning claims: %v", winners, outcomes)
	}
	record, err := store.Payment(ctx, id)
	if err != nil || record.Payment.Claim == nil || record.Payment.Version != 4 {
		t.Fatalf("claimed payment: %+v %v", record.Payment, err)
	}
	// Replaying the winning claim commits nothing.
	winner := claims[0]
	if record.Payment.Claim.Kind == domain.CancellationClaim {
		winner = claims[1]
	}
	if replayed, err := apply("CLAIM", winner); err != nil || replayed.Version != 4 {
		t.Fatalf("claim replay: %+v %v", replayed, err)
	}

	owner := domain.ExecutionWorkflowID(id, domain.BookTransfer)
	owned, err := apply("ASSIGN_EXECUTION_OWNER", func(payment domain.Payment) (domain.Payment, *domain.Change, error) {
		return payment.AssignExecutionOwner(payment.Version, owner, worker, time.Now())
	})
	if err != nil || owned.ExecutionWorkflowID != owner || owned.Version != 5 {
		t.Fatalf("owner: %+v %v", owned, err)
	}
	changes, err := store.Changes(ctx, id)
	if err != nil || len(changes) != 5 {
		t.Fatalf("history: %d %v", len(changes), err)
	}
	for index, change := range changes {
		if change.Version != int64(index+1) || change.PolicyVersion == "" || change.Actor.Subject == "" {
			t.Fatalf("history row %d: %+v", index, change)
		}
	}
	var sequences, audits int
	if err := pool.QueryRow(ctx, `SELECT max(event_sequence), (SELECT count(*) FROM app.audit_entries WHERE aggregate_id = $1) FROM app.domain_events WHERE aggregate_id = $1`,
		string(id)).Scan(&sequences, &audits); err != nil {
		t.Fatal(err)
	}
	if sequences != 2 || audits != 5 {
		t.Fatalf("events/audits: %d %d", sequences, audits)
	}
}

func TestPostgresDatabaseGuardsProtectHistory(t *testing.T) {
	store, pool := openTestStore(t)
	ctx := context.Background()
	receipt, err := store.Receive(ctx, newReceipt(t, newID(t), "guards-1", 100))
	if err != nil {
		t.Fatal(err)
	}
	id := string(receipt.Payment.ID)
	for name, statement := range map[string]string{
		"instruction update": `UPDATE app.payment_instructions SET amount_minor = 1 WHERE payment_id = $1`,
		"idempotency delete": `DELETE FROM app.idempotency_records WHERE payment_id = $1`,
		"history delete":     `DELETE FROM app.payment_versions WHERE payment_id = $1`,
		"event update":       `UPDATE app.domain_events SET payload = '{}' WHERE aggregate_id = $1`,
		"audit delete":       `DELETE FROM app.audit_entries WHERE aggregate_id = $1`,
		"payment delete":     `DELETE FROM app.payments WHERE payment_id = $1`,
		"version skip":       `UPDATE app.payments SET version = version + 2 WHERE payment_id = $1`,
		"customer change":    `UPDATE app.payments SET customer_id = gen_random_uuid(), version = version + 1 WHERE payment_id = $1`,
		"unrecorded version": `UPDATE app.payments SET version = version + 1 WHERE payment_id = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, id); err == nil {
			t.Errorf("%s was permitted", name)
		}
	}
}
