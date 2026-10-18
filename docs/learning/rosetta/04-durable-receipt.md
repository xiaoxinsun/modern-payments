# Rosetta 04: the durable receipt and idempotency

**Payments idea.** `POST /api/v1/payments` answers `202` only after everything
that describes the payment has committed in **one** database transaction:
- the payment and its immutable instruction;
- version 1 of its history;
- the command;
- the idempotency record;
- an audit entry;
- the `PaymentReceived` event and its outbox row;
- a pending workflow dispatch.

If any insert fails, none of them exist. The `Idempotency-Key` makes a
customer's retry return the original receipt, and a changed payload under the
same key is a `409`. Two layers enforce this:
- the application service replays *without calling Core Banking*, so a customer
  can recover a lost reply even while the ledger is down;
- the store re-checks under a lock, so two identical requests arriving at the
  same instant produce one payment.

Files: `backend/internal/payment/application/service.go` (use case),
`payment/storage/store.go` (transaction), `payment/domain/instruction.go`
(canonical hash). Design: ADR 0009.

## Go: the use case

From `backend/internal/payment/application/service.go:139`:

```go
func (service *Service) Submit(ctx context.Context, request SubmitRequest) (Receipt, error) {
	instruction := request.Instruction
	if !request.Principal.HasRole(CustomerRole) || !request.Principal.CanAccessCustomer(string(instruction.CustomerID)) {
		return Receipt{}, fmt.Errorf("%w: customer scope denied", ErrForbidden)
	}
	scope := domain.IdempotencyScope{Channel: domain.CustomerAPI, CustomerID: instruction.CustomerID, Key: request.IdempotencyKey}
	// …
	hash, err := instruction.Hash()
	// …
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
```

The service depends only on two small interfaces it declares itself, `Store`
and `AccountDirectory` (see [go-for-java §6](../go-for-java.md)).

## Go: the one transaction

From `backend/internal/payment/storage/store.go:106`:

```go
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
		// …
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
```

`store.failpoint` is a function field that does nothing in production. The
integration test replaces it to fail after each step in turn, then asserts that
all nine tables are still empty.

## Java 21

```java
interface PaymentStore {
    Optional<IdempotencyRecord> findIdempotency(IdempotencyScope scope);
    Receipt receipt(Id paymentId, Id commandId);
    Receipt receive(NewReceipt receipt);
}

final class PaymentService {
    private final PaymentStore store;
    private final AccountDirectory accounts;

    Receipt submit(SubmitRequest req) {
        var instruction = req.instruction();
        if (!req.principal().hasRole("customer") || !req.principal().canAccessCustomer(instruction.customerId()))
            throw new ForbiddenException("customer scope denied");
        var scope = new IdempotencyScope(Channel.CUSTOMER_API, instruction.customerId(), req.idempotencyKey());
        String hash = instruction.hash();
        var existing = store.findIdempotency(scope);          // replay needs no Core Banking call
        if (existing.isPresent()) {
            if (!existing.get().requestHash().equals(hash)) throw new IdempotencyConflictException();
            return store.receipt(existing.get().paymentId(), existing.get().commandId()).asReplay();
        }
        authorizeAccounts(instruction);
        return store.receive(new NewReceipt(scope, instruction, req.correlationId(),
                                           new Actor(ActorType.CUSTOMER, req.principal().subject())));
    }
}

// JdbcPaymentStore.receive
record Step(String name, SqlRunnable run) {}
Receipt receive(NewReceipt r) throws SQLException {
    try (Connection c = dataSource.getConnection()) {
        c.setAutoCommit(false);
        try {
            lockIdempotencyScope(c, r.scope());               // pg_advisory_xact_lock(hash(scope))
            var existing = findIdempotency(c, r.scope());
            if (existing.isPresent()) { /* conflict or replay, then commit */ }
            List<Step> steps = List.of(
                new Step("payment", () -> insertPayment(c, payment)),
                new Step("instruction", () -> insertInstruction(c, payment, r.instruction())) /* , … */);
            for (Step s : steps) {
                try { s.run().run(); }
                catch (SQLException e) { throw new SQLException("receive " + s.name(), e); }
                failpoint.accept(s.name());
            }
            c.commit();
            return new Receipt(payment, commandId, Dispatch.pending(payment.id()), false);
        } catch (Exception e) { c.rollback(); throw e; }
    }
}
```

## Why the Go looks like this

- **The interface lives with the use case, not the implementation.**
  `application.Store` is declared in `application`, and `storage.Store`
  satisfies it without naming it. The storage package imports application
  *types*, never the other way round. That is the "ports and adapters" shape
  without any framework.
- **`receipt, err := …; receipt.Replayed = true; return receipt, err`.** Setting
  a field on a possibly-zero value before checking `err` looks odd to Java eyes.
  It is harmless: when `err != nil`, callers ignore the receipt. Go code
  routinely returns a zero-ish value alongside an error.
- **Closures capture variables, not values.** The `func() error` literals in
  `steps` capture `ctx`, `queries` and `payment` by reference, like Java lambdas
  capturing effectively-final locals. Go does not require the variables to be
  final.
- **`fmt.Errorf("receive %s: %w", step.name, err)`** adds which step failed
  without losing the underlying PostgreSQL error, which the HTTP layer still
  finds with `errors.As(err, &pgError)` and maps to a retryable 503.
- **A function-valued field instead of a mock framework.** `failpoint func(step
  string) error` is set in `NewStore` to a no-op and replaced by tests in the
  same package. There is no Mockito; plain function values and small interfaces
  cover most cases.
- **Advisory locks are ordinary SQL.** `LockIdempotencyScope` is
  `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, generated by sqlc
  like any query. The lock is released automatically at commit or rollback.

## Try it

```sh
make payment-integration      # atomicity at each of 8 failure points, 16 concurrent duplicates
```

Then do steps 1–3 of the [B02 walkthrough](../../walkthroughs/b02-payment-commands.md),
and look at the nine tables in its database step.
