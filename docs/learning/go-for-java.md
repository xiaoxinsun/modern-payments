# Go for Java developers, using this codebase

Each section shows a Go idiom as it appears in GTB, followed by the Java you
would probably have written, and why Go does it differently. Go excerpts are
verbatim, with `// …` marking omitted lines. The Java is plain Java 21 and is
illustrative only.

The one idea behind most of what follows: **Go prefers explicit, boring code
over hidden machinery.** There is no DI container, no annotations that change
behaviour, no exceptions, no inheritance and no runtime proxies. The price is
more lines. The payoff is that everything a function does is visible in that
function.

---

## 1. Packages, visibility and files

Go has no `public`, `private` or `protected`. **A capitalised name is exported
from its package, and a lowercase name is private to it.** The unit of
encapsulation is the *package* (a directory), not the class.

From `backend/internal/payment/domain/payment.go:47`:

```go
func (state LifecycleState) cancellable() bool {
	switch state {
	case Received, Validating, Scheduled, RequiresReview, Accepted:
		return true
	}
	return false
}

func (state LifecycleState) Terminal() bool {
	return state == Rejected || state == Cancelled || state == Returned
}
```

`cancellable` is usable only inside `package domain`, while `Terminal` is
public. Every file in the directory shares one namespace, so `payment.go` calls
the helpers in `instruction.go` with no import.

A directory named `internal/` can be imported only by code under its parent.
That is how `gtb.local/backend/internal/...` stays private to this module, like
a Java module that exports nothing. `go.mod` plays the part of `pom.xml`: the
module path, the Go version and pinned dependencies, with `go.sum` as the lock
file.

---

## 2. Named types and enums

Go has no `enum`. You define a named type and typed constants:

From `backend/internal/payment/domain/payment.go:19`:

```go
type LifecycleState string

const (
	Received       LifecycleState = "RECEIVED"
	Validating     LifecycleState = "VALIDATING"
	// …
	Returned       LifecycleState = "RETURNED"
)
```

```java
enum LifecycleState { RECEIVED, VALIDATING, /* … */ RETURNED;
    boolean terminal() { return this == REJECTED || this == CANCELLED || this == RETURNED; } }
```

The Go version is *not* closed: `LifecycleState("BANANA")` compiles. Validity is
checked where values enter the program, for example by `Validate()` methods or
JSON decoding. Named types still stop you from passing a `Currency` where a
`LifecycleState` is expected. `value.ID` is the same trick: a `string` that the
compiler will not mix up with other strings.

---

## 3. Structs and methods instead of classes

A struct is data only. Methods are declared separately, with a **receiver**:

From `backend/internal/platform/value/money.go:34`:

```go
type Money struct {
	AmountMinor int64    `json:"amountMinor,string"`
	Currency    Currency `json:"currency"`
}
// …
func (m Money) Add(other Money) (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if err := other.Validate(); err != nil {
		return Money{}, err
	}
	if m.Currency != other.Currency {
		return Money{}, errors.New("currency mismatch")
	}
	// Both inputs are bounded to 18 digits; their sum cannot overflow int64.
	result := Money{AmountMinor: m.AmountMinor + other.AmountMinor, Currency: m.Currency}
	return result, result.Validate()
}
```

```java
record Money(long amountMinor, Currency currency) {
    Money add(Money other) {            // throws on mismatch
        validate(); other.validate();
        if (currency != other.currency) throw new IllegalArgumentException("currency mismatch");
        return new Money(Math.addExact(amountMinor, other.amountMinor), currency);
    }
}
```

**Value receiver vs. pointer receiver:**
- `func (m Money)` gets a *copy*. It cannot mutate the caller's value, so `Money`
  behaves like an immutable Java record.
- `func (m *Money) UnmarshalJSON` takes a pointer, because decoding must write
  into the caller's variable.
- Services use pointer receivers (`func (service *Service)`), because they hold
  shared state such as a connection pool and should not be copied.

A rule of thumb: small immutable data uses values; things with identity or
state use pointers.

There are no constructors. `NewService(...)` is only a naming convention for a
function that returns an initialised struct. `Money{}` is the **zero value**:
every field holds its type's zero (`0`, `""`, `nil`, `false`). Go code is
designed so that zero values are safe, which is why so many functions return
`Money{}, err`.

---

## 4. Errors are values, not exceptions

This is the biggest adjustment. A function that can fail returns `error` as its
**last result**, and the caller checks it immediately:

From `backend/internal/payment/application/service.go:139`:

```go
func (service *Service) Submit(ctx context.Context, request SubmitRequest) (Receipt, error) {
	instruction := request.Instruction
	if !request.Principal.HasRole(CustomerRole) || !request.Principal.CanAccessCustomer(string(instruction.CustomerID)) {
		return Receipt{}, fmt.Errorf("%w: customer scope denied", ErrForbidden)
	}
	// …
	hash, err := instruction.Hash()
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
```

- **Sentinel errors** are package-level values, the Go equivalent of an
  exception type you can catch:

  From `backend/internal/payment/application/service.go:15`:

  ```go
  var (
  	ErrInvalidRequest        = errors.New("invalid payment request")
  	ErrForbidden             = errors.New("payment access denied")
  	ErrNotFound              = errors.New("payment record not found")
  	ErrIdempotencyConflict   = errors.New("idempotency key was reused with a different payload")
  ```

- **Wrapping.** `fmt.Errorf("%w: …", ErrForbidden)` adds context and keeps the
  original inside, like `new XException(msg, cause)`.
- **Matching.** `errors.Is(err, ErrForbidden)` walks the wrap chain, like a
  `catch (ForbiddenException e)` that also matches causes. The HTTP layer maps
  errors to status codes in one place, in `payment/api/handler.go` `writeError`.
- **Typed errors.** When you need data from an error, define a type with an
  `Error() string` method and extract it with `errors.As`:

  From `backend/internal/projections/projector.go:106`:

  ```go
  type errPoison struct{ reason string }

  func (err errPoison) Error() string { return err.reason }
  ```

  From `backend/internal/projections/projector.go:291`:

  ```go
  	var poison errPoison
  	var pgErr *pgconn.PgError
  	switch {
  	case err == nil:
  		return true, savepoint.Commit(ctx)
  	case errors.As(err, &poison):
  	case errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")):
  ```

  ```java
  try { write(...); savepoint.release(); return true; }
  catch (PoisonException e) { reason = e.reason(); }
  catch (SQLException e) { if (!e.getSQLState().startsWith("22") && !e.getSQLState().startsWith("23")) throw e; reason = "…"; }
  ```

Why Go works this way: every failure path is visible at the call site, so you
cannot forget that a line can fail, and control flow never jumps. `panic`
exists, but it is reserved for programmer bugs. See `mustUUID` in the storage
packages, which panics only if unvalidated data reaches it.

---

## 5. Multiple return values and the "found" idiom

Go functions return several values, so there is no `Optional` wrapper and no
out-parameter:

From `backend/internal/payment/application/service.go:157`:

```go
	if existing, found, err := service.store.FindIdempotency(ctx, scope); err != nil {
		return Receipt{}, err
	} else if found {
		if existing.RequestHash != hash {
			return Receipt{}, ErrIdempotencyConflict
		}
```

```java
Optional<IdempotencyRecord> existing = store.findIdempotency(scope);   // SQLException propagates
if (existing.isPresent()) { … }
```

`if x, err := f(); err != nil { … }` declares variables scoped to that `if`
statement. You will see it everywhere.

---

## 6. Interfaces are implicit and small

A type satisfies an interface just by having the methods, with no
`implements`. By convention the **consumer** declares the interface it needs,
and keeps it small:

From `backend/internal/payment/application/service.go:39`:

```go
type AccountDirectory interface {
	// Account returns ErrAccountNotFound for unknown accounts and
	// ErrDependencyUnavailable when ownership cannot be established.
	Account(context.Context, value.ID) (Account, error)
}
```

The HTTP client in another package happens to have that method:

From `backend/internal/adapters/corebanking/client.go:49`:

```go
func (client *Client) Account(ctx context.Context, accountID value.ID) (application.Account, error) {
```

The application layer never imports the adapter. Tests pass a fake that has
the same method. In Java you would write `class CoreBankingClient implements
AccountDirectory`, which couples the implementation to the interface's package.
In Go the dependency points only one way. Other examples are
`application.Store`, `eventing.OutboxSource` and `projections.Journal`.

---

## 7. Dependency injection is `main()`

There is no container. Each `cmd/<service>/main.go` builds the object graph by
hand and passes dependencies into constructors:

From `backend/cmd/payment-api/main.go:65`:

```go
	store := storage.NewStore(pool)
```

From `backend/cmd/payment-api/main.go:95`:

```go
	if err := api.Register(mux, verifier, application.NewService(store, accounts), store); err != nil {
```

`store` is passed twice: once as an `application.Store` and once as an
`application.Journal`, because it implements both. What Spring does with
`@Autowired` and component scanning is about 50 readable lines here. Read each
`main.go` first to understand a service.

---

## 8. `context.Context`: cancellation, deadlines and request data

Nearly every function that does I/O takes `ctx context.Context` as its first
parameter. The context carries:
- a **cancellation signal**: SIGTERM cancels the root context, and every query,
  HTTP call and loop below it stops;
- a **deadline**;
- request-scoped values such as the correlation ID and the authenticated
  principal.

From `backend/internal/eventing/relay.go:93`:

```go
func (relay *Relay) RunOnce(ctx context.Context) (DeliveryResult, error) {
	return relay.Source.Deliver(ctx, relay.BatchSize, func(ctx context.Context, events []contracts.Event) []error {
		produceContext, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return relay.Publisher.Publish(produceContext, relay.Topic, events)
	})
}
```

Java spreads these jobs across thread interrupts, `Future.get(timeout)`,
per-client timeout settings and `ThreadLocal`s (MDC, `SecurityContextHolder`).
Go passes one explicit parameter down the call chain. The request's values ride
on it too: see `auth.PrincipalFromContext` and `runtime.CorrelationID`.

---

## 9. `defer`, and transactions without `@Transactional`

`defer` schedules a call to run when the surrounding function returns, on
every path. It replaces `try/finally`:

From `backend/internal/corebanking/service.go:172`:

```go
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Hold{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := coredb.New(tx)
```

The pattern is: begin; defer a rollback; do the work; `tx.Commit(ctx)` at the
end. A rollback after a commit is a harmless no-op, so any early `return` rolls
back automatically.

```java
try (Connection c = dataSource.getConnection()) {
    c.setAutoCommit(false);
    try { … ; c.commit(); } catch (Exception e) { c.rollback(); throw e; }
}
```

With `@Transactional`, a proxy does this invisibly, and self-invocation or a
checked exception can silently change the behaviour. In Go the transaction
boundary is plain code you can see.

---

## 10. Pointers as "optional" and `nil`

Go has pointers but no pointer arithmetic. They are used for sharing, for
mutation and, most visibly here, **to mean "may be absent"**:

From `backend/internal/payment/domain/payment.go:123`:

```go
type Payment struct {
	ID                  value.ID
	CustomerID          value.ID
	// …
	ExecutionWorkflowID string
	Claim               *Claim
```

`Claim *Claim` is `nil` until a submission or cancellation is claimed, like
`Optional<Claim>` or a nullable field. Plain value fields (`string`, `int64`)
cannot be `nil`; their "absent" form is the zero value. Dereferencing a nil
pointer panics, which is Go's `NullPointerException`, so code checks
`payment.Claim != nil` first.

---

## 11. Slices, maps and `range`

- A **slice** (`[]Entry`) is Go's `ArrayList`: it has a length, a capacity and
  `append`.
- A **map** (`map[K]V`) is a `HashMap`. Iteration order is deliberately random.
- **Sets** are written `map[K]struct{}` (an empty struct takes zero bytes).

From `backend/internal/corebanking/domain/model.go:166`:

```go
func SortedAccountIDs(entries []Entry) []value.ID {
	set := make(map[value.ID]struct{}, len(entries))
	for _, entry := range entries {
		set[entry.AccountID] = struct{}{}
	}
	ids := make([]value.ID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return strings.Compare(string(ids[i]), string(ids[j])) < 0 })
	return ids
}
```

```java
static List<String> sortedAccountIds(List<Entry> entries) {
    return entries.stream().map(Entry::accountId).distinct().sorted().toList();
}
```

Go has no streams; loops are the idiom. (Generic helpers exist since Go 1.18,
see §14, but explicit loops remain normal.) The comment explains the payments
reason for this function: locking accounts in a fixed order prevents
deadlocks between concurrent postings.

---

## 12. `switch` without a condition

A bare `switch` evaluates each `case` as a boolean, top to bottom, and runs the
first true one. There is no fall-through. It reads like a guard table:

From `backend/internal/payment/domain/payment.go:255`:

```go
	switch {
	case claimed(CancellationClaim) && to != Cancelled:
		return fmt.Errorf("%w: a cancellation claim permits only cancellation", ErrInvalidTransition)
	// …
	case to == Rejected && (outcome == Pending || outcome == Unknown || outcome == ConfirmedSuccess):
		// A timeout or unknown outcome is never rejection evidence.
		return fmt.Errorf("%w: rejection after submission requires confirmed failure", ErrInvalidTransition)
```

In Java this would be an `if / else if` chain.

---

## 13. Goroutines, channels and `select`

- `go f()` starts a **goroutine**: a very cheap thread that Go multiplexes onto
  OS threads.
- **Channels** pass values between goroutines, and closing a channel is a
  broadcast "done" signal.
- `select` waits on several channel operations at once.

From `backend/cmd/payment-worker/main.go:61`:

```go
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay.Run(ctx) }()
	defer func() { stop(); <-relayDone }()
```

This runs the relay in the background. On shutdown it cancels the context
(`stop()`), then blocks until the relay goroutine closes `relayDone`.

```java
ExecutorService executor = Executors.newSingleThreadExecutor();
Future<?> relayDone = executor.submit(() -> relay.run(stopSignal));
// on shutdown:
stopSignal.cancel(); relayDone.get();
```

The relay's sleep between polls must also wake when shutdown starts, which
`select` handles:

From `backend/internal/eventing/relay.go:81`:

```go
		if wait > 0 {
			select {
			case <-ctx.Done():
				relay.Logger.Info("outbox relay stopped", "relay", relay.Name)
				return
			case <-time.After(wait):
			}
		}
```

Java would use `Thread.sleep` and catch `InterruptedException`, or a
`ScheduledExecutorService`. Shared counters in tests use `sync/atomic` (as in
`eventing_test.go`), the equivalent of `AtomicInteger`. Run `make race`: Go's
race detector finds unsynchronised access at test time.

---

## 14. Generics, used sparingly

From `backend/internal/payment/domain/payment.go:386`:

```go
func contains[T comparable](values []T, wanted T) bool {
	for _, candidate := range values {
		if candidate == wanted {
			return true
		}
	}
	return false
}
```

`comparable` is a constraint: any type that supports `==`. It is like
`<T> boolean contains(List<T> values, T wanted)`. Go generics have no wildcards
or variance, and idiomatic code uses them only when they remove real
duplication.

---

## 15. Closures and "step tables"

Functions are values. The durable receipt lists its eight inserts as a table
of named closures, so a test can inject a failure after any step:

From `backend/internal/payment/storage/store.go:130`:

```go
	steps := []struct {
		name string
		run  func() error
	}{
		{"payment", func() error { return insertPayment(ctx, queries, payment) }},
		{"instruction", func() error { return insertInstruction(ctx, queries, payment, receipt.Instruction) }},
```

`[]struct{ … }{ … }` is an anonymous struct type used once, inline. Java's
closest equivalent is `List<Map.Entry<String, Callable<Void>>>`, or a small
local `record Step(String name, Runnable run)`.

---

## 16. JSON with struct tags

The backtick strings after fields are **tags**: metadata read at runtime by
`encoding/json` (and by other libraries). `json:"amountMinor,string"` renames
the field and encodes the `int64` as a JSON string. A type can take over its
own encoding by defining `MarshalJSON` and `UnmarshalJSON`:

From `backend/internal/platform/value/money.go:75`:

```go
func (m *Money) UnmarshalJSON(data []byte) error {
	var wire struct {
		AmountMinor string   `json:"amountMinor"`
		Currency    Currency `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
```

This is Jackson's `@JsonProperty` plus a custom `JsonDeserializer`, with the
decoder rejecting unknown fields.

---

## 17. Generated code: sqlc and oapi-codegen, not JPA

GTB writes SQL by hand, and `sqlc` generates type-safe Go functions from it:

From `backend/internal/payment/storage/queries.sql:4`:

```sql
-- name: GetIdempotencyRecord :one
SELECT channel, customer_id, idempotency_key, request_hash, payment_id, command_id, recorded_at
FROM app.idempotency_records
WHERE channel = $1 AND customer_id = $2 AND idempotency_key = $3;
```

From `backend/internal/payment/storage/paymentdb/queries.sql.go:49`:

```go
func (q *Queries) GetIdempotencyRecord(ctx context.Context, arg GetIdempotencyRecordParams) (AppIdempotencyRecord, error) {
	row := q.db.QueryRow(ctx, getIdempotencyRecord, arg.Channel, arg.CustomerID, arg.IdempotencyKey)
```

There is no entity manager, lazy loading, dirty checking or generated SQL.
It is closest to hand-written JDBC or jOOQ, but with the boilerplate generated.
`paymentdb.New(tx)` or `paymentdb.New(pool)` runs the same queries inside or
outside a transaction, because both satisfy the tiny `DBTX` interface (§6).
`oapi-codegen` does the same for HTTP: it generates request and response types
and a router from the OpenAPI contract.

---

## 18. Tests: `testing`, table-driven, `t.Helper()`

Tests live beside the code in `*_test.go` files. They are plain functions
`TestXxx(t *testing.T)`, run with `go test ./...`. There are no annotations and
no assertion library: you compare, then call `t.Fatalf` or `t.Errorf`.
**Table-driven tests** are the house style:

From `backend/internal/payment/domain/domain_test.go:57`:

```go
	invalid := map[string]Instruction{
		"zero amount":        mutate(func(i *Instruction) { i.Amount.AmountMinor = 0 }),
		"negative amount":    mutate(func(i *Instruction) { i.Amount.AmountMinor = -1 }),
		"unsupported ccy":    mutate(func(i *Instruction) { i.Amount.Currency = "JPY" }),
		// …
	}
	for name, instruction := range invalid {
		if err := instruction.Validate(); !errors.Is(err, ErrInvalidInstruction) {
			t.Errorf("%s: got %v", name, err)
		}
	}
```

- This is JUnit 5 `@ParameterizedTest` with `@MethodSource`.
- `t.Run(name, …)` makes each row a named subtest.
- `t.Helper()` in a helper makes failures point at the caller's line.
- Tests that need a real database skip themselves unless an environment
  variable is set; the Python runners in `infra/compose/tests/` set it.

Useful commands:

```sh
cd backend
go test ./internal/payment/domain/                   # one package
go test -run TestUnknownOutcome -v ./internal/payment/domain/
go test -race ./...                                  # with the race detector
go doc ./internal/payment/domain Payment.Transition  # docs from the command line
```

---

## Quick translation table

| Java | Go in this repo |
| --- | --- |
| `class` with fields and methods | `struct` + methods with receivers |
| `interface X` + `implements X` | small interface declared by the consumer; satisfied implicitly |
| `enum` | named type + typed `const` block |
| exceptions, `try/catch` | `error` return, `if err != nil`, `errors.Is/As` |
| `Optional<T>` / `null` | extra `found bool` result, or `*T` and `nil` |
| `try/finally` | `defer` |
| `@Transactional` | `BeginTx` + `defer Rollback` + `Commit` |
| Spring DI | construction in `cmd/*/main.go` |
| `ThreadLocal`, MDC, `SecurityContext` | `context.Context` values |
| `ExecutorService`, `Future` | `go`, channels, `select` |
| `AtomicInteger`, `synchronized` | `sync/atomic`, `sync.Mutex` |
| JPA / Hibernate | SQL + sqlc-generated functions |
| Jackson annotations | struct tags, `MarshalJSON` / `UnmarshalJSON` |
| Maven `pom.xml` | `go.mod` / `go.sum` |
| JUnit `@ParameterizedTest` | table-driven tests, `t.Run` |
| servlet filter | `func(http.Handler) http.Handler` middleware ([slice 05](rosetta/05-http-boundary.md)) |
