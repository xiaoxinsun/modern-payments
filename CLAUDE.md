# CLAUDE.md

This repository holds the **GTB (Global Trust Bank) reference implementation**: a
runnable local payment platform that accompanies the completed book in
`manuscript/`. The book is finished. Treat `manuscript/`, `appendices/`, `notes/`
and `SUMMARY.md` as read-only background, and don't edit them unless asked.

## Response style

Be a concise expert. Skip preambles, pleasantries and recaps; give the shortest
viable response. This applies to chat only: checkpoints, evidence and handoffs
stay as complete as the protocol requires.

## Start every session here

1. Read the resume pointer and phase status register in `spec/gtb-implementation.md`.
   This is the only source of phase status.
2. Open the active phase packet in `spec/phases/<id>.md`. Read only the sections of
   `spec/gtb-foundation.md` that the packet names, not the whole foundation.
3. Run `git status`. Uncommitted work may be the previous session's checkpoint.
   Keep it, and reconcile it against the checkpoint before making new changes.
4. Follow the working protocol in `gtb-implementation.md`. Above all, checkpoint
   the packet before stopping, and never record a check as passed unless it ran.

## Source-of-truth documents

| Path | Owns |
| --- | --- |
| `spec/gtb-foundation.md` | Architecture, business rules, accounting scenarios A–E, invariants, acceptance scenarios |
| `spec/gtb-implementation.md` | Phase order and status, resume pointer, dependency adoption register, transition log |
| `spec/phases/*.md` | Per-phase scope, exit criteria, checkpoint, completion handoff |
| `spec/adr/*.md` | Architecture decisions; add or amend an ADR for any material change |
| `contracts/` | Versioned OpenAPI, event and simulator JSON schemas, plus examples |

## Commands (run from the repo root)

```sh
make deps                 # download pinned Go modules
make check                # gate: toolchain, fmt-check, mod-check, vet, test, race, build
make sqlc-generate        # regenerate sqlc code after changing queries or migrations (pinned sqlc)
make infra-init infra-up  # generate local .env credentials; start Compose stack
make db-provision db-migrate db-seed
make infra-status | infra-verify | infra-restart-check | auth-smoke
make corebanking-integration corebanking-smoke   # B01 live checks
make payment-integration payment-smoke           # B02 live checks
make kafka-topics eventing-integration eventing-smoke   # B03 live checks (stop any walkthrough event-projector first)
make infra-reset CONFIRM=delete-gtb-data         # destructive: deletes volumes; only when asked
```

- Go **1.27.1** exactly, with `GOTOOLCHAIN=local`. Binaries are built to the ignored `.build/bin/`.
- Tests that need a live database skip unless their env var is set, for example
  `GTB_COREBANKING_TEST_DATABASE_URL`. Use the Python runners in
  `infra/compose/tests/`, which build these URLs from `.env` without printing credentials.
- If an unrelated test fails, report it. Don't claim a gate passed when it didn't run.

## Architecture rules

- Layout:
  - One Go module `gtb.local/backend` with nine binaries in `backend/cmd/`.
  - Each service's code lives under `backend/internal/<service>/`.
  - Shared code is in `internal/platform/{value,auth,runtime,telemetry}`; shared
    outbox relay and Kafka mechanics are in `internal/eventing` (B03).
- Domain packages (`*/domain`) must not import `net/*`, `database/*`, Temporal or
  outer GTB packages. The only GTB package they may use is `internal/platform/value`.
  `internal/architecture/dependencies_test.go` enforces this.
- Data ownership:
  - Each of `payment`, `corebanking`, `projections` and `simulators` has its own
    database and owner login.
  - There are no cross-domain table access and no cross-service transactions.
  - Services integrate only through idempotent APIs, events and outboxes.
- Migrations live in `backend/migrations/<db>/NNNN_name.sql`, numbered consecutively.
  **Never edit an applied migration**: the runner checks SHA-256 history and fails.
  Make any fix in a new migration.
- Libraries (ADR 0007 and the dependency register):
  - Use the maintained library the register assigns to the current phase. Pin an
    exact version, and record that version in the packet.
  - Don't add modules that nothing uses yet.
  - Money, posting, idempotency, authorization, workflow state and
    unknown-outcome rules stay as project-owned code.
- HTTP:
  - Use `net/http` plus the platform runtime: correlation IDs, the error envelope
    `{error:{code,message,retryable},correlationId}`, readiness and redacted `slog` JSON logs.
  - Use `auth.Authenticate` with role and scope middleware on every non-health route.
  - Keep OIDC audience validation enabled.

## Financial invariants

Break none of these (from foundation §5):

- Money is `value.Money`: int64 minor units with validated currency (SGD/USD), and
  never floats. On the wire it's `{"amountMinor":"<decimal string>","currency":"SGD"}`.
- Every posting balances within each currency. Posted entries and operations are
  append-only. Corrections are new linked postings.
- Each financial operation has one stable caller-supplied operation ID. Repeating it
  with the same payload replays the original result. Repeating it with a different
  payload returns 409. Never mint a fresh ID as a way to retry.
- Reserve funds atomically, with no overdraft. Consuming a hold and posting commit in one transaction.
- An unknown external outcome stays unknown: no blind resubmission, refund,
  reroute or hold release without evidence.
- Read models never authorize money movement. A workflow or HTTP success is not proof of completion.

## Safety and hygiene

- Credentials exist only in the ignored `infra/compose/.env` (mode 0600). Never print,
  commit or log them, and never store them in checkpoints or evidence.
- Keep Compose volumes and generated credentials. Normal startup must never reset data.
- Commit only when asked. The existing style is one commit per completed phase,
  such as `phase Infra-03 implemented`.
