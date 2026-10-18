# Rosetta 06: the outbox relay

**Payments idea.** The payment and its `PaymentReceived` event were committed
together, the event as an `outbox` row ([slice 04](04-durable-receipt.md)). A
**relay** now moves outbox rows to Kafka:
- It marks a row delivered **only after Kafka acknowledges** it, in the same
  database transaction that locked the row. A crash anywhere therefore leads to
  a republish, never a lost event: delivery is *at least once*, with the same
  `eventId` each time.
- Two relays never publish the same row at once, because
  `FOR UPDATE SKIP LOCKED` makes each skip the other's locked rows.
- While Kafka is down, rows accumulate with their failed attempts recorded,
  and the relay backs off.

Files: `backend/internal/eventing/relay.go` (the loop, shared by payment and
Core Banking), `eventing/kafka.go` (franz-go producer),
`payment/storage/journal.go` (`Deliver`), `cmd/payment-worker/main.go`
(wiring). Design: ADR 0010.

## Go: one delivery batch (storage)

From `backend/internal/payment/storage/journal.go:22`:

```go
func (store *Store) Deliver(ctx context.Context, limit int, publish func(context.Context, []contracts.Event) []error) (eventing.DeliveryResult, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return eventing.DeliveryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := paymentdb.New(tx)
	rows, err := queries.LockPendingOutbox(ctx, int32(limit))
	if err != nil || len(rows) == 0 {
		return eventing.DeliveryResult{}, err
	}
	// …
	results := publish(ctx, events)
	var published []uuid.UUID
	var result eventing.DeliveryResult
	for index, publishErr := range results {
		if publishErr == nil {
			published = append(published, rows[index].EventID)
			result.Published++
			continue
		}
		result.Failed++
		if err := queries.MarkOutboxFailed(ctx, paymentdb.MarkOutboxFailedParams{
			LastError: optional(eventing.DeliveryError(publishErr)), EventID: rows[index].EventID,
		}); err != nil {
			return eventing.DeliveryResult{}, err
		}
	}
	// …
	return result, tx.Commit(ctx)
}
```

`LockPendingOutbox` selects undelivered rows in journal order with
`FOR UPDATE OF o SKIP LOCKED`. The `publish` parameter is a *function* passed in
by the relay, so storage knows nothing about Kafka.

## Go: publishing, keyed by aggregate

From `backend/internal/eventing/kafka.go:77`:

```go
func (publisher KafkaPublisher) Publish(ctx context.Context, topic string, events []contracts.Event) []error {
	results := make([]error, len(events))
	records := make([]*kgo.Record, 0, len(events))
	index := make(map[*kgo.Record]int, len(events))
	for position, event := range events {
		record, err := EventRecord(topic, event)
		if err != nil {
			results[position] = err
			continue
		}
		index[record] = position
		records = append(records, record)
	}
	// …
	// ProduceSync reports results in completion order; map them back by record.
	for _, result := range publisher.Client.ProduceSync(ctx, records...) {
		results[index[result.Record]] = result.Err
	}
	return results
}
```

## Go: the loop, backoff and shutdown

From `backend/internal/eventing/relay.go:54`:

```go
	for {
		result, err := relay.RunOnce(ctx)
		if ctx.Err() != nil {
			relay.Logger.Info("outbox relay stopped", "relay", relay.Name)
			return
		}
		// …
		wait := relay.Idle
		switch {
		case err != nil || result.Failed > 0:
			// …
			backoff = min(max(backoff*2, relay.Idle), relay.MaxBackoff)
			wait = backoff
		case result.Published > 0:
			backoff = relay.Idle
			// …
			if result.Published == relay.BatchSize {
				wait = 0
			}
		default:
			backoff = relay.Idle
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				relay.Logger.Info("outbox relay stopped", "relay", relay.Name)
				return
			case <-time.After(wait):
			}
		}
	}
```

The worker runs it in a goroutine and waits for it on shutdown:

From `backend/cmd/payment-worker/main.go:61`:

```go
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay.Run(ctx) }()
	defer func() { stop(); <-relayDone }()
```

## Java 21

```java
interface OutboxSource {
    DeliveryResult deliver(int limit, Function<List<Event>, List<Exception>> publish) throws SQLException;
}

final class Relay implements Runnable {
    private final OutboxSource source; private final Publisher publisher;
    private final Duration idle, maxBackoff; private final int batchSize;
    private volatile boolean stopping;

    public void run() {
        Duration backoff = idle;
        while (!stopping) {
            Duration wait = idle;
            try {
                var result = source.deliver(batchSize, events -> publisher.publish(topic, events, Duration.ofSeconds(15)));
                if (result.failed() > 0) { backoff = min(max(backoff.multipliedBy(2), idle), maxBackoff); wait = backoff; }
                else if (result.published() == batchSize) wait = Duration.ZERO;
                else backoff = idle;
            } catch (SQLException e) {
                backoff = min(max(backoff.multipliedBy(2), idle), maxBackoff); wait = backoff;
            }
            try { if (!wait.isZero()) Thread.sleep(wait); }
            catch (InterruptedException e) { Thread.currentThread().interrupt(); return; }
        }
    }
}

// JdbcPaymentOutbox.deliver: lock, publish, record results, commit.
DeliveryResult deliver(int limit, Function<List<Event>, List<Exception>> publish) throws SQLException {
    try (Connection c = dataSource.getConnection()) {
        c.setAutoCommit(false);
        try {
            List<OutboxRow> rows = lockPending(c, limit);        // FOR UPDATE OF o SKIP LOCKED
            if (rows.isEmpty()) { c.rollback(); return DeliveryResult.EMPTY; }
            List<Exception> errors = publish.apply(rows.stream().map(OutboxRow::event).toList());
            /* mark published / failed per row, as in the Go */
            c.commit();
            return result;
        } catch (Exception e) { c.rollback(); throw e; }
    }
}

// wiring
var executor = Executors.newSingleThreadExecutor();
Future<?> relayDone = executor.submit(relay);
Runtime.getRuntime().addShutdownHook(new Thread(() -> { relay.stop(); executor.shutdownNow(); }));
```

## Why the Go looks like this

- **A function parameter as a seam.** `Deliver(ctx, limit, publish func(...))`
  lets the storage layer own the transaction while the relay owns Kafka. In
  Java you would pass a `Function` the same way. Go just makes it cheap and
  common.
- **`[]error` in input order.** Each event gets its own result, so a partial
  failure marks only the failed rows. `nil` in the slice means success.
- **`map[*kgo.Record]int` keyed by pointer.** Pointers are comparable in Go, so
  a map from pointer to index is a quick identity map, like
  `IdentityHashMap<Record, Integer>`.
- **`select` on `ctx.Done()` and a timer** makes the sleep interruptible by
  shutdown. That replaces `Thread.sleep` plus `InterruptedException`, and it is
  why every loop in GTB takes a context.
- **Built-in `min` and `max`** (Go 1.21+) work on any ordered type, here
  `time.Duration`, which is just an `int64` of nanoseconds.
- **Bounded waits on purpose.** Holding row locks while waiting on Kafka is
  safe only because each wait is bounded (`context.WithTimeout` in `RunOnce`,
  plus the producer's 10s delivery timeout). Without the bound, a broker outage
  would pin a database transaction open indefinitely: a real bug found during
  B03, described in ADR 0010.
- **Goroutine + channel + `defer`** replaces an executor, a future and a
  shutdown hook. `defer close(relayDone)` guarantees the done signal even if
  `Run` panics.

## Try it

```sh
cd backend && go test -v -run Relay ./internal/eventing/
make eventing-integration     # includes TestScratchOutboxMarksOnlyAcknowledgedEvents, concurrent relays
```

Then do steps 1, 4 and 5 of the [B03 walkthrough](../../walkthroughs/b03-events-and-queries.md)
to watch `published_at` and `delivery_attempts` change while Kafka is stopped.
