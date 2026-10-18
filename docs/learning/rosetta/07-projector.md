# Rosetta 07: the projector (CQRS read side)

**Payments idea.** Customers and operators search payments through **query
models** (`payment_summary`, `payment_timeline`) in a separate database, built
from events. Kafka delivers at least once and can reorder, so the projector
must:
- **deduplicate** by event ID (the inbox);
- **reorder** using each payment's `eventSequence`: early events wait in a
  buffer until the gap fills;
- **quarantine poison** (events it can never project) instead of stopping;
- commit projection changes and the Kafka checkpoint **in one transaction**,
  so a crash can cause only redelivery, which the inbox absorbs.

Files: `backend/internal/projections/projector.go` (`Process`, `project`,
`apply`), `consumer.go` (Kafka loop), `catchup.go` (gap filling from the
journal), `rebuild.go`. Tables: `backend/migrations/projections/0002_*.sql`.
Design: ADR 0010.

## Go: dedupe, then buffer or apply, then drain

From `backend/internal/projections/projector.go:180`:

```go
func (store *Store) project(ctx context.Context, tx pgx.Tx, consumer string, target projectiondb.OpenGenerationsRow, item Incoming, result *Result) error {
	queries := projectiondb.New(tx)
	event := *item.Event
	aggregateID := mustUUID(event.AggregateID)
	if err := queries.LockAggregate(ctx, fmt.Sprintf("%d|%s", target.Generation, event.AggregateID)); err != nil {
		return err
	}
	duplicate, err := queries.InboxContains(ctx, projectiondb.InboxContainsParams{Generation: target.Generation, EventID: mustUUID(event.EventID)})
	if err != nil {
		return err
	}
	if duplicate {
		result.Duplicates++
		return nil
	}
	last, err := store.progress(ctx, queries, target.Generation, aggregateID)
	if err != nil {
		return err
	}
	switch {
	case event.EventSequence <= last:
		result.Poisoned++
		return store.repair(ctx, queries, consumer, item, &event, fmt.Sprintf("sequence %d already projected by a different event", event.EventSequence))
	case event.EventSequence > last+1:
		// …
		stored, err := queries.BufferEvent(ctx, projectiondb.BufferEventParams{
		// …
	}

	next := item
	for {
		applied, err := store.apply(ctx, tx, consumer, target.Generation, next)
		if err != nil {
			return err
		}
		if !applied {
			result.Poisoned++
			return nil
		}
		result.Projected++
		// …
		successor, found, err := store.takeBuffered(ctx, queries, consumer, target.Generation, aggregateID, next.Event.EventSequence+1, result)
		if err != nil || !found {
			return err
		}
		next = successor
	}
}
```

## Go: a savepoint turns bad data into a repair entry

A constraint violation would abort the whole PostgreSQL transaction, and with
it the batch. Each event is therefore written inside a **savepoint**, and a
data problem rolls back only that event:

From `backend/internal/projections/projector.go:283`:

```go
func (store *Store) apply(ctx context.Context, tx pgx.Tx, consumer string, generation int32, item Incoming) (bool, error) {
	event := *item.Event
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = savepoint.Rollback(ctx) }()
	err = store.write(ctx, projectiondb.New(savepoint), generation, item)
	var poison errPoison
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return true, savepoint.Commit(ctx)
	case errors.As(err, &poison):
	case errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")):
		poison = errPoison{reason: "projection rejected by schema: " + pgErr.Code}
	default:
		return false, err
	}
	if err := savepoint.Rollback(ctx); err != nil {
		return false, err
	}
	return false, store.repair(ctx, projectiondb.New(tx), consumer, item, &event, poison.reason)
}
```

`tx.Begin(ctx)` on a transaction creates a SAVEPOINT (pgx's nested
transaction). SQLSTATE class 22 is a data exception and class 23 is an
integrity-constraint violation. Any other error, such as a lost connection,
fails the whole batch so it can be retried.

## Go: the consumer loop

From `backend/internal/projections/consumer.go:92`:

```go
	for {
		fetches := live.Client.PollRecords(ctx, 500)
		if ctx.Err() != nil {
			live.Logger.Info("projection consumer stopped", "consumer", live.Name)
			return
		}
		// …
		var items []Incoming
		var records []*kgo.Record
		fetches.EachPartition(func(partition kgo.FetchTopicPartition) {
			for _, record := range partition.Records {
				records = append(records, record)
				items = append(items, Decode(record.Value, eventing.DeliveryLive, &KafkaPosition{
					Topic: record.Topic, Partition: record.Partition, Offset: record.Offset, HighWatermark: partition.HighWatermark,
				}))
			}
		})
		// …
		result, ok := live.process(ctx, items)
		if !ok {
			return
		}
		// …
		runEffects(ctx, live.Logger, live.Effects, result.Applied)
		if err := live.Client.CommitRecords(ctx, records...); err != nil && ctx.Err() == nil {
			live.Logger.Warn("Kafka offset commit failed; the database checkpoint is authoritative", "error", err)
		}
		live.Client.AllowRebalance()
	}
```

`live.process` calls `Store.Process`, retrying the *same* batch with backoff
until the database accepts it. Only then are offsets committed to Kafka, and
even those are informational: on restart the projector resumes from the
offsets it stored in its own database.

## Java 21

```java
sealed interface Outcome permits Projected, Duplicate, Buffered, Poisoned {}

Outcome project(Connection c, int generation, Incoming item) throws SQLException {
    Event e = item.event();
    lockAggregate(c, generation + "|" + e.aggregateId());          // pg_advisory_xact_lock
    if (inboxContains(c, generation, e.eventId())) return new Duplicate();
    long last = progress(c, generation, e.aggregateId());          // 0 when unseen
    if (e.eventSequence() <= last) { repair(c, item, "sequence already projected"); return new Poisoned(); }
    if (e.eventSequence() > last + 1) { bufferEvent(c, generation, item); return new Buffered(); }

    Incoming next = item;
    while (true) {
        if (!apply(c, generation, next)) return new Poisoned();
        Optional<Incoming> successor = takeBuffered(c, generation, e.aggregateId(), next.event().eventSequence() + 1);
        if (successor.isEmpty()) return new Projected();
        next = successor.get();
    }
}

boolean apply(Connection c, int generation, Incoming item) throws SQLException {
    Savepoint sp = c.setSavepoint();
    try {
        write(c, generation, item);                                // summary, timeline, inbox
        c.releaseSavepoint(sp);
        return true;
    } catch (PoisonException e) {
        c.rollback(sp); repair(c, item, e.reason()); return false;
    } catch (SQLException e) {
        String state = e.getSQLState();
        if (state == null || !(state.startsWith("22") || state.startsWith("23"))) throw e;
        c.rollback(sp); repair(c, item, "projection rejected by schema: " + state); return false;
    }
}

// Consumer loop with the Kafka Java client
while (!stopping) {
    ConsumerRecords<String, byte[]> records = consumer.poll(Duration.ofMillis(500));
    if (records.isEmpty()) continue;
    List<Incoming> items = new ArrayList<>();
    for (var r : records) items.add(decode(r.value(), Delivery.LIVE, new KafkaPosition(r.topic(), r.partition(), r.offset())));
    Result result = retryUntilCommitted(() -> store.process(CONSUMER, items));   // one DB transaction
    runEffects(result.applied());
    consumer.commitAsync();                                                      // informational only
}
```

## Why the Go looks like this

- **Counters instead of a sealed result type.** `project` updates a shared
  `*Result` (`result.Duplicates++`) and returns only `error`, so one batch
  accumulates totals across events and generations. The Java version returns
  an `Outcome` per event. Both are reasonable; passing a pointer to accumulate
  into is common in Go.
- **Typed error plus `errors.As`** replaces multiple `catch` blocks.
  `var poison errPoison; errors.As(err, &poison)` both tests the type and
  extracts the value. Note the empty `case errors.As(err, &poison):` body: the
  match itself fills `poison`, and execution continues after the switch.
- **Savepoints are nested transactions.** pgx exposes a SAVEPOINT as
  `tx.Begin`, with the same `defer Rollback` / `Commit` shape as a top-level
  transaction.
- **`event := *item.Event`** copies the struct out of the pointer, so later
  code cannot accidentally change the caller's event.
- **Closures over loop state.** The `EachPartition(func(...){...})` callback
  appends to `items` and `records`, which are declared outside it, exactly like
  a Java lambda, except that Go allows mutating captured variables.
- **`for { … }`** is Go's `while (true)`. Go has only one loop keyword, `for`.
- **Replay safety is a data flag, not a code path.** Each event carries a
  `Delivery` (`LIVE` or `REPLAY`), and `runEffects` withholds `REPLAY`
  deliveries from any consumer marked effectful. A rebuild can therefore reuse
  the exact same projection code without re-sending anything.

## Try it

```sh
cd backend && go test -v -run 'Replay|Decode|Compare' ./internal/projections/
make eventing-integration     # dedupe, gap buffer, poison, gap catch-up, rebuild (scratch databases)
```

Then do steps 3–7 of the [B03 walkthrough](../../walkthroughs/b03-events-and-queries.md).
