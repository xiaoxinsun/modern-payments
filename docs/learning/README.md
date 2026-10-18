# Learning track: payments and Go, for Java developers

This repository is a teaching codebase: it exists so you can learn the payments
domain and the Go language together, in preparation for Go work on a real
payments platform. The walkthroughs show you how to *run* what each phase built.
This track helps you *read* it.

It covers phases B01–B03: the ledger, payment commands, and event delivery with
queries. The Java in these pages is plain modern Java 21 (records, sealed
interfaces, JDBC, no framework), chosen to line up with the Go line by line.
Java listings are illustrative. They are not compiled, and they skip imports
and trivial helpers.

## Reading order

1. **[Payments primer](payments-primer.md).** The domain concepts, each pointing
   to the code that implements it. Read this first: the code makes much more
   sense once you know why a payment is "received" and not "accepted".
2. **[Go for Java developers](go-for-java.md).** Go's idioms, each illustrated
   with a real snippet from this codebase and the Java you would have written.
   Keep it open as a reference while you read the slices.
3. **The Rosetta slices.** Seven side-by-side translations of the most important
   code, in the order the data flows:

   | Slice | Payment idea | Go idea |
   | --- | --- | --- |
   | [01 Money and IDs](rosetta/01-money-and-ids.md) | Minor units, no floats | Named types, value receivers, JSON codecs |
   | [02 Ledger posting](rosetta/02-ledger-posting.md) | Holds, double entry, operation IDs | Explicit transactions, `defer`, maps |
   | [03 Payment state machine](rosetta/03-payment-state-machine.md) | Lifecycle, claims, evidence | Immutable values, multiple returns, error wrapping |
   | [04 Durable receipt](rosetta/04-durable-receipt.md) | Idempotency keys, 202 means received | Small interfaces, closures, step tables |
   | [05 HTTP boundary](rosetta/05-http-boundary.md) | Scoped authorization | `net/http` handlers and middleware |
   | [06 Outbox relay](rosetta/06-outbox-relay.md) | At-least-once delivery | Goroutines, `select`, contexts, backoff |
   | [07 Projector](rosetta/07-projector.md) | CQRS, dedupe, ordering gaps | Savepoints, `errors.As`, typed errors |

4. **The [walkthroughs](../walkthroughs/README.md).** Run what you just read,
   and watch the rows change.
5. **[Exercises](exercises.md).** Small changes you make yourself, with the
   existing tests as guardrails.

## How to read Go code in this repository

- Start at a service's `backend/cmd/<service>/main.go`. It wires everything by
  hand, so it is the table of contents for that process.
- Then read inward: `api` (HTTP) → `application` (use cases) → `domain` (rules)
  → `storage` (SQL). The domain package imports nothing from the outer layers,
  and a test enforces that.
- Skip generated files on a first read: `*.gen.go`, `storage/*db/*.go` and
  `*.bundle.json`. Read the SQL in `queries.sql` and the OpenAPI contract
  instead; they are the sources the Go was generated from.
- Excerpts in this track are quoted verbatim from the code, with `// …` marking
  lines left out. Each excerpt names its file and first line, so you can open
  the whole function.
