# Rosetta 02: ledger reservation and posting

**Payments idea.** Core Banking is the bank's ledger.
- A **hold** reserves funds, lowering *available* balance but not *booked*.
- A **posting** is a balanced set of debit and credit lines.
- Every call carries a caller-chosen **operation ID**. The same ID and payload
  replays the stored result, while the same ID with a different payload is a
  conflict (409).
- Reservations can never overdraw, and concurrent postings must not deadlock.
- The posting, its event and its outbox row commit together (see
  [slice 06](06-outbox-relay.md)).

Files: `backend/internal/corebanking/service.go` (transactions),
`corebanking/domain/model.go` (pure accounting rules),
`corebanking/storage/queries.sql` (SQL). Design: ADR 0008.

## Go: the pure rule (domain)

From `backend/internal/corebanking/domain/model.go:105`:

```go
func ValidateEntries(entries []Entry) error {
	if len(entries) < 2 || len(entries) > 64 {
		return errors.New("posting must contain 2 to 64 entries")
	}
	type totals struct{ debit, credit int64 }
	byCurrency := make(map[value.Currency]totals)
	for _, entry := range entries {
		// …
		current := byCurrency[entry.Amount.Currency]
		if entry.Side == Debit {
			// …
			current.debit += entry.Amount.AmountMinor
		} else {
			// …
			current.credit += entry.Amount.AmountMinor
		}
		byCurrency[entry.Amount.Currency] = current
	}
	for _, current := range byCurrency {
		if current.debit != current.credit {
			return ErrUnbalanced
		}
	}
	return nil
}
```

## Go: an idempotent, locked reservation (service)

From `backend/internal/corebanking/service.go:172`:

```go
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
	// …
	active, err := queries.SumActiveHolds(ctx, accountID)
	if err != nil {
		return domain.Hold{}, err
	}
	if active > account.BookedBalanceMinor || request.Amount.AmountMinor > account.BookedBalanceMinor-active {
		return domain.Hold{}, ErrInsufficientFunds
	}
```

`LockOperationIdentity` is a PostgreSQL transaction-scoped advisory lock on the
operation ID. `LockAccount` is `SELECT … FOR UPDATE`. Together they serialize
duplicates and concurrent spends of the same account.

## Go: locking in a stable order, then computing balances

From `backend/internal/corebanking/service.go:420`:

```go
	accounts := make(map[value.ID]coredb.AppAccount, len(request.Entries))
	for _, accountValueID := range domain.SortedAccountIDs(request.Entries) {
		accountID, _ := parseID(accountValueID)
		row, err := queries.LockAccount(ctx, accountID)
		// …
		accounts[accountValueID] = row
	}
	// …
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
```

## Java 21 (plain JDBC)

```java
Hold reserve(ReserveRequest req) throws SQLException {
    validate(req);
    String hash = requestHash(req.accountId(), req.amount(), req.expiresAt());
    try (Connection c = dataSource.getConnection()) {
        c.setAutoCommit(false);
        try {
            lockOperationIdentity(c, req.operationId());            // pg_advisory_xact_lock
            Optional<Operation> existing = findOperation(c, req.operationId());
            if (existing.isPresent()) {
                Operation op = existing.get();
                if (!op.type().equals("RESERVE") || !op.requestHash().equals(hash))
                    throw new ConflictException();                   // -> 409
                Hold replay = getHold(c, op.resourceId());
                c.commit();
                return replay;
            }
            Account account = lockAccount(c, req.accountId())       // SELECT … FOR UPDATE
                .orElseThrow(NotFoundException::new);
            long active = sumActiveHolds(c, req.accountId());
            if (active > account.booked() || req.amount().amountMinor() > account.booked() - active)
                throw new InsufficientFundsException();
            Hold hold = insertOperationAndHold(c, req, hash);
            c.commit();
            return hold;
        } catch (Exception e) {
            c.rollback();
            throw e;
        }
    }
}

static void validateEntries(List<Entry> entries) {
    Map<Currency, long[]> totals = new EnumMap<>(Currency.class);  // [debit, credit]
    for (Entry e : entries) {
        long[] t = totals.computeIfAbsent(e.amount().currency(), k -> new long[2]);
        t[e.side() == Side.DEBIT ? 0 : 1] += e.amount().amountMinor();
    }
    for (long[] t : totals.values())
        if (t[0] != t[1]) throw new UnbalancedPostingException();
}
```

## Why the Go looks like this

- **The transaction is plain code.** `BeginTx` + `defer Rollback` + an explicit
  `Commit` on each success path (including the replay path) replaces
  `@Transactional`. The replay path commits too, to release the advisory lock
  promptly. A rollback would have released it as well.
- **`if existing, found, err := …; err != nil {} else if found {}`** keeps the
  three outcomes (failure, found, not found) in one statement, where Java would
  use `Optional` plus exceptions.
- **Sentinel errors carry the business outcome** (`ErrConflict`,
  `ErrInsufficientFunds`, `ErrInvalidState`), and `writeBankingError` in
  `corebanking/http.go` maps them to HTTP statuses in one switch (404, 409,
  400, and 503 for database errors). In Java these would be exception
  subclasses caught by an `@ControllerAdvice`.
- **`accountID, _ := parseID(...)`** ignores an error on purpose (`_` is the
  blank identifier), because the ID was validated at the top of the function.
  Reviewers treat `_` for an error as a claim that needs justifying.
- **A local type inside a function** (`type totals struct{ debit, credit int64 }`)
  is legal and common. It is scoped like a Java local record.
- **Map values are copies.** `current := byCurrency[...]` copies the struct, so
  the code writes it back with `byCurrency[...] = current`. Java's
  `computeIfAbsent` returns a live reference instead; this difference often
  surprises Java developers.
- **A missing map key reads as the zero value.** `balances[id]` for an unknown
  key returns `0` with no exception. Use `v, ok := m[k]` when absence matters.
- **Pure rules live in `domain`**, with no database types, so they are unit
  tested without PostgreSQL (`corebanking/domain/model_test.go`).

## Try it

```sh
cd backend && go test -v ./internal/corebanking/domain/
make corebanking-integration          # scenarios A–E, concurrency, lost reply, 409 (live DB)
```

Then do steps 2–4 of the [B01 walkthrough](../../walkthroughs/b01-core-banking.md).
