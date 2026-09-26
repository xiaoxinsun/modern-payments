# Database migrations

`make db-provision`, `make db-migrate`, then `make db-seed` operates on the running
local stack. Provisioning is explicit and rerunnable on existing volumes; it does
not replace passwords or databases. Conflicting existing owners or elevated role
attributes fail the command. `make infra-init` adds missing credential keys
without changing existing values. Keep credentials with their volumes.

Each directory owns a separate PostgreSQL database and login with the same name:
`payment`, `corebanking`, `projections`, `simulators`. Migration and seed commands
connect over authenticated TCP as that owner, never as `gtb_admin`. Temporal's
two databases and Keycloak's database use upstream schema tooling instead.

Add consecutive `NNNN_description.sql` files starting at `0001` within the owning
directory. Never change or remove an applied migration. The runner locks each
database, compares the recorded SHA-256 manifest against all source files, and
applies pending files plus their ledger entries in one transaction. Concurrent
runners serialize; errors roll back the entire pending set. Each database commits
independently, so a failure in a later domain does not undo an earlier domain.
Fix the unapplied migration and rerun. The ledger is `_gtb.schema_migrations`.

Migration files are trusted SQL maintained with the repository. Do not include
transaction control, psql metacommands, or commands prohibited in transactions
(such as `CREATE INDEX CONCURRENTLY`); plan those changes separately. There is no
automatic down migration, cross-database transaction or checksum repair command.
The owner can administer its own database; these credentials are local owner
credentials, not a claim that future runtime roles enforce append-only audit.
Feature phases must establish narrower grants where their invariants require it.

Version `0001_local_scaffold.sql` creates an empty `app` schema and a synthetic
seed manifest under `_gtb`. `backend/seeds/local.sql` inserts one stable
`local-i02-v1` marker per domain, preserving its timestamp on reruns. This proves
persistence and seed idempotency without prematurely defining accounts, postings
or payment tables. Business fixtures arrive with their owning feature phases.

`make infra-verify` runs live health, credential, isolation, migration replay,
concurrency, checksum rejection and rollback checks. `make infra-restart-check`
recreates containers and checks application seeds, Temporal/OIDC state and Kafka
record persistence. See the [local runbook](../../docs/runbooks/local-development.md).
