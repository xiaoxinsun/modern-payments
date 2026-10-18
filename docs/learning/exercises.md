# Exercises

Small changes you make yourself, with the existing tests as guardrails. They
start easy and end with one feature that touches every layer. Work on a branch
(`git switch -c learning/<name>`) and discard it afterwards. These are for
practice, not for merging.

Each exercise lists the files to read, what to do, and how to check your work.
The check commands pass on the current tree before you start, so a failure
afterwards is yours to explain. Exercises marked **(live)** need the local
stack from the [walkthrough setup](../walkthroughs/README.md).

---

## 1. Money arithmetic (value receivers, errors, table-driven tests)

**Read** `backend/internal/platform/value/money.go` and `money_test.go`, and
[Rosetta 01](rosetta/01-money-and-ids.md).

**Do** Add `func (m Money) Subtract(other Money) (Money, error)`. It must
reject mixed currencies and results outside `±MaxMinorUnits`, like `Add`. Then
write a table-driven test covering same currency, mismatch and bounds.

**Check**

```sh
cd backend && go test -v -run Money ./internal/platform/value/
```

**Hint** Can you implement it with `Add` and a negated copy? What happens when
you negate `-MaxMinorUnits`? (The bound is symmetric on purpose.)

---

## 2. The lifecycle graph as a test (maps, loops, `errors.Is`)

**Read** `backend/internal/payment/domain/payment.go` (`lifecycleTransitions`,
`Transition`, `guardTransition`) and [Rosetta 03](rosetta/03-payment-state-machine.md).

**Do** In `domain_test.go`, write `TestTerminalStatesHaveNoExits`. For each of
`Rejected`, `Cancelled` and `Returned`, build a payment in that state and
assert that every attempted `Transition` returns `ErrInvalidTransition`. Build
the payment directly as a struct literal: tests in the same package can set
unexported fields and skip the business path.

**Check**

```sh
cd backend && go test -v -run Terminal ./internal/payment/domain/
```

**Hint** `for _, to := range []LifecycleState{…}`. Then ask: which states
should be terminal in *payments* terms, and why is `COMPLETED` not in the
list? (See `Completed: {Returned}` and the primer's section on returns.)

---

## 3. Follow an error from SQL to HTTP (error wrapping)

**Read** `payment/storage/store.go` `Receive`, `application/service.go`
`Submit`, and `payment/api/handler.go` `writeError`.

**Do** Trace `ErrIdempotencyConflict` from where it is created to the HTTP
status and error code. Write down each function it passes through. Then add a
case to `TestResponsesMatchOpenAPIContract` (or a new test) asserting that the
409 body has `"retryable":false`, while the `ErrDependencyUnavailable` 503 has
`"retryable":true`.

**Check**

```sh
cd backend && go test -v -run Responses ./internal/payment/api/
```

**Hint** Decode `response.Body.Bytes()` into
`struct{ Error struct{ Retryable bool } }`. Why must a retryable 503 tell the
client to reuse the *same* `Idempotency-Key`?

---

## 4. Out-of-order delivery (live; projector internals)

**Read** [Rosetta 07](rosetta/07-projector.md) and the helpers at the top of
`backend/internal/projections/postgres_integration_test.go` (`paymentEvents`,
`deliveries`, `process`, `state`).

**Do** Write `TestScratchReverseOrderDeliveryProjectsInOrder`: build a payment
with 5 events, deliver them one at a time in reverse order (5, 4, 3, 2, 1),
then assert:
- the timeline is `[1 2 3 4 5]`;
- the version is 5;
- nothing remains buffered;
- the inbox holds 5 events.

Then deliver all five again, and assert that the result is 5 duplicates.

**Check**

```sh
make eventing-integration        # runs every TestScratch* test against throwaway databases
```

**Hint** Test names must start with `TestScratch` for the runner to pick them
up. Which `Result` field counts events that were already waiting in the
buffer?

---

## 5. See a data race (goroutines, `sync.Mutex`, `-race`)

**Read** `backend/internal/eventing/eventing_test.go` (`scriptedSource` uses
`atomic.Int32`) and [go-for-java §13](go-for-java.md).

**Do** Create a throwaway `backend/internal/eventing/race_scratch_test.go`:
1. Start 4 goroutines that each append 1000 items to one shared slice, and
   wait for them with a `sync.WaitGroup`.
2. Run the test with `-race` and read the report.
3. Fix it with a `sync.Mutex`, then again with a channel that a single
   goroutine drains.
4. Delete the file afterwards.

**Check**

```sh
cd backend && go test -race -run Scratch -v ./internal/eventing/
```

**Hint** Compare the channel version with Java's `BlockingQueue` plus a single
consumer thread. Which version makes the ownership of the slice obvious?

---

## 6. Trace one request across services (live; context and correlation IDs)

**Read** `platform/runtime/http.go` (`WithCorrelation`),
`adapters/paymentreceipts/client.go`, and step 3 of the
[B03 walkthrough](../walkthroughs/b03-events-and-queries.md).

**Do**
1. With terminals A–E running, stop the projector (D).
2. Submit a payment, then call the query API for it with your own header
   `-H "X-Correlation-ID: $(newid)"`.
3. Find that ID in the query-api log *and* the payment-api log. The query API
   forwarded it with your bearer token when it fell back to the receipt.
4. Explain which `context.Context` carried the ID from one place to the other.

**Check** Both terminals show an access-log line with the same
`correlation_id`.

---

## 7. Capstone: filter payments by product (every layer)

**Read** `contracts/openapi/v1/query.json`,
`backend/internal/projections/storage/queries.sql` (`ListActivePayments`),
`projections/query/service.go` (`List`, `cursorScope`),
`projections/api/handler.go` (`ListPayments`) and their tests.

**Do** Add an optional `product` query parameter to `GET /api/v1/payments`:
1. Contract: add the parameter, reusing `payment.json#/components/schemas/Product`,
   then run `make openapi-generate`.
2. SQL: add a nullable filter like the `lifecycle_state` one, then run
   `make sqlc-generate`.
3. Service: add the field to `ListRequest`, and bind it into the cursor scope.
   Why must the cursor include it?
4. Handler: pass it through.
5. Tests: add a contract-validation case in `api/handler_test.go` and a filter
   case in `query/postgres_integration_test.go`.
6. Try it with curl, as in walkthrough step 2.

**Check**

```sh
make check                   # gofmt, vet, unit and contract tests, embedded-bundle freshness
make eventing-integration    # the query-service test runs through the read-only login
```

**Hint** If `make check` says a bundle is stale, you edited the contract
without regenerating. If the integration test says `permission denied`, you
are querying through `query_reader`, which can read only what migration
`0002` grants. A new migration would be needed for a new table, but not for a
new filter.
