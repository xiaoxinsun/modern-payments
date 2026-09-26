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

## Inspect the local PostgreSQL instance

From the repository root, open an interactive administrator session with the
stable Compose service name:

```sh
docker compose \
  --env-file infra/compose/.env \
  -f infra/compose/compose.yaml \
  exec postgres psql -U gtb_admin -d postgres
```

Plain `docker exec` works too, but it requires the generated container name and
explicit interactive flags:

```sh
docker exec -it gtb-postgres-1 psql -U gtb_admin -d postgres
```

`-i` keeps standard input open and `-t` allocates the terminal used by the
`psql` prompt. Without them, `psql` receives end-of-input and exits successfully
without displaying a prompt. The `gtb-postgres-1` name can change with the
Compose project name or scaling, whereas the Compose service name `postgres` is
stable. For one non-interactive query, no terminal is needed:

```sh
docker exec gtb-postgres-1 psql -U gtb_admin -d postgres -c '\l+'
```

These in-container commands use PostgreSQL's trusted local maintenance socket;
they do not test password authentication. With a host `psql` installation, use
the loopback TCP endpoint and generated administrator password instead:

```sh
PGPASSWORD=$(sed -n 's/^POSTGRES_ADMIN_PASSWORD=//p' infra/compose/.env) \
  psql -h 127.0.0.1 -p 5432 -U gtb_admin -d postgres
```

For DBeaver, DataGrip or pgAdmin, use host `127.0.0.1`, port `5432`, initial
database `postgres`, user `gtb_admin`, the `POSTGRES_ADMIN_PASSWORD` value from
the ignored `infra/compose/.env`, and no SSL. PostgreSQL connects to one database
per session, even though this bootstrap superuser can inspect all of them.

| Database | Owner/login | Contents |
| --- | --- | --- |
| `postgres` | `gtb_admin` | Bootstrap administration. |
| `payment` | `payment` | Payment command domain. |
| `corebanking` | `corebanking` | Accounts and posting authority. |
| `projections` | `projections` | Query/read models. |
| `simulators` | `simulators` | External-system simulator records. |
| `temporal` | `temporal` | Temporal workflow persistence. |
| `temporal_visibility` | `temporal` | Temporal visibility/search data. |
| `oidc` | `oidc` | Keycloak identities and configuration. |

The four application databases contain `app`, `_gtb` and the standard `public`
schema. `app` is intentionally empty until its feature phase. `_gtb` contains
`schema_migrations` and `seed_manifest`; public access to application databases
and schemas is restricted. Temporal and Keycloak manage their own tables. System
schemas such as `pg_catalog` and `information_schema` also exist in every database.

Useful commands inside `psql` are:

```text
\l+                         -- list databases
\du+                        -- list roles
\c payment                  -- connect to another database
\dn+                        -- list schemas in the current database
\dt app.*                   -- list business tables
\dt _gtb.*                  -- list migration/seed metadata tables
TABLE _gtb.schema_migrations;
TABLE _gtb.seed_manifest;
```

The Keycloak bootstrap administrator is also named `gtb_admin`, but it is a
separate identity using `OIDC_ADMIN_PASSWORD`; it is not the PostgreSQL role.

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
