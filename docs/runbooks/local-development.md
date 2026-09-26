# Local GTB dependencies

Run commands from the repository root. Install Docker with Compose v2.20 or
newer and Python 3; start the Docker daemon first. This stack runs infrastructure
dependencies; application services are started separately. `payment-api` contains
the I03 authenticated runtime, while business services remain feature scaffolds.

## Start and inspect

```sh
make infra-init
make infra-config
make infra-up
make db-provision
make db-migrate
make db-seed
make infra-status
```

`infra-init` generates random local passwords in ignored `infra/compose/.env`
with mode 0600. Repeating it preserves existing values and adds missing keys
when the scaffold gains new owners. Keep it with the data volumes:
changing environment passwords does not rotate existing database passwords.
Do not commit this file or attach expanded `docker compose config` output to
reports. `infra-config` validates without displaying credentials.

`infra-up` waits up to 300 seconds for health checks after image pulls. PostgreSQL
must accept TCP connections before schema setup; Temporal's container probe checks
TCP and its namespace job checks the health RPC before namespace creation.
Schema and namespace jobs must exit successfully.
The UI, Kafka and Keycloak each have their own health check. Diagnose failures
with `make infra-status` and targeted logs, for example:

```sh
docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml logs --tail 80 temporal-schema temporal
```

Published ports bind only to IPv4 loopback. Use `localhost` for the OIDC issuer
and Kafka advertised host address.

| Dependency | Host endpoint | Compose endpoint / durable state |
| --- | --- | --- |
| PostgreSQL | `localhost:5432` | `postgres:5432`; `gtb_postgres-data` |
| Temporal | `localhost:7233`, namespace `gtb` | `temporal:7233`; PostgreSQL `temporal` and `temporal_visibility` databases |
| Temporal UI | <http://localhost:8081> | `temporal-ui:8080`; no independent durable state |
| Kafka | `localhost:9092` | `kafka:19092`; `gtb_kafka-data` |
| OIDC / Keycloak | <http://localhost:8080> | `oidc:8080`; PostgreSQL `oidc` database |

The PostgreSQL bootstrap administrator and Keycloak bootstrap administrator are
named `gtb_admin`; their passwords are distinct entries in the local `.env`.
Temporal and OIDC use their own non-superuser database roles. Their initial
database creation runs only on an empty PostgreSQL volume. Application owners
are provisioned explicitly by `db-provision`, including on an existing volume.
Provisioning never drops a database or rotates an existing password. Conflicting
owners or elevated role attributes fail instead of silently taking over objects.

| Database / login | Local credential key | Purpose |
| --- | --- | --- |
| `payment` | `PAYMENT_DB_PASSWORD` | Payment command domain |
| `corebanking` | `COREBANKING_DB_PASSWORD` | Independent Core Banking authority |
| `projections` | `PROJECTIONS_DB_PASSWORD` | Query projections |
| `simulators` | `SIMULATORS_DB_PASSWORD` | Independent simulator records |
| `temporal`, `temporal_visibility` / `temporal` | `TEMPORAL_DB_PASSWORD` | Workflow histories and visibility |
| `oidc` | `OIDC_DB_PASSWORD` | Keycloak configuration and identities |

Owner roles have no superuser, role-creation, database-creation, replication or
RLS-bypass privileges and no memberships in other owners. PUBLIC cannot connect
to these databases. Connect applications to their own database only, never with
`gtb_admin`. The CLI uses the authenticated `postgres` service address; trusted
local maintenance sockets must not be used to test application authentication.
Local owner roles can administer their own domain; narrower runtime grants,
including append-only audit permissions, arrive with the owning features.

`db-migrate` applies versioned SQL as each domain owner, under a per-database
transaction lock with SHA-256 history validation. `db-seed` writes one synthetic
infrastructure marker per domain; reruns preserve data and timestamps. These
commands do not create customer accounts or process payments. Read the
[migration guide](../../backend/migrations/README.md) before adding feature SQL.

Temporal schema setup is rerunnable at the pinned version; namespace `gtb` has
72-hour history retention. Kafka uses one KRaft broker/controller with replication
factor one, persistent log storage and explicit topic creation. Keycloak uses
development mode backed by PostgreSQL. These are local dependencies; the GTB
realm, customer/operator identities, clients and service identities are supplied
by I03.

## Stop, restart and reset

```sh
make infra-down
make infra-up
```

Ordinary stop/down retains named volumes and credentials. Do not change the
Compose project name (`gtb`) unless intentionally creating a different stack.
Only run this separate **destructive reset** when all local data may be deleted:

```sh
make infra-reset CONFIRM=delete-gtb-data
make infra-up
make db-provision db-migrate db-seed
```

This deletes GTB database contents, workflow histories, broker logs and OIDC
configuration. Reset requires the explicit confirmation string and is never a
dependency of startup, provisioning, migration or seeding. It retains local
credentials. Startup recreates infrastructure schemas and the Temporal namespace;
the following commands recreate application scaffolds and synthetic markers.
Never use reset to repair a failed migration
when existing data matters. Port conflicts should be resolved before startup;
do not stop unrelated projects to free ports.

## Verify and resume

```sh
make infra-verify
make infra-restart-check
```

`infra-verify` checks all service health states, owner writes, password rejection,
all 41 forbidden role/database pairs (including the admin database), lack of
elevated role attributes/memberships, seed idempotency, concurrent migration
replay, changed-checksum rejection and atomic migration rollback. It creates
only rolled-back test tables. `infra-restart-check` deliberately performs an
ordinary down/up, verifies the four domains' exact seed/ledger records plus
OIDC/Temporal identity persistence, and checks a temporary Kafka message. It
removes its temporary topic afterward. Neither command resets volumes.

For an interrupted setup: inspect `make infra-status`, rerun `make infra-up`,
then repeat provisioning, migration and seed commands. Missing/changed applied
migrations require restoring their original files; do not edit ledger checksums.
If credentials are lost, recover the matching `.env` or use an explicit local
administrative password rotation; merely generating new passwords cannot unlock
existing volumes.

## Local identity and authenticated runtime

The versioned Keycloak `gtb` realm imports on first startup. It contains synthetic
customer, separate operations maker/checker, separate compliance reviewer/approver,
auditor and platform administrator users. Confidential rail, screening, Core
Banking and projector service identities are also seeded. All passwords and client
secrets come from the ignored mode-0600 `.env`; never print, copy into source, or
attach that file to evidence. The `gtb-cli` password grant exists only for local
automated smoke checks, and Keycloak `start-dev` is not a production configuration.

Run the authenticated sample runtime and its checks with:

```sh
make auth-smoke
```

The check starts `payment-api` on loopback port 18082, verifies public liveness and
OIDC-backed readiness, then exercises valid, invalid and expired tokens; customer
scope; operator/compliance/auditor roles; integration service roles; correlation
IDs; disabled development controls; structured credential-safe logs; and SIGTERM
shutdown. It deletes its temporary log. The normal runtime defaults to loopback
port 8082 and issuer `http://localhost:8080/realms/gtb`; configure deployments with
`GTB_HTTP_ADDRESS`, `GTB_PROFILE`, `GTB_AUTH_ISSUER`, `GTB_AUTH_AUDIENCE` and, when
needed, `GTB_AUTH_JWKS_URL`. Development controls additionally require
`GTB_ENABLE_DEV_CONTROLS=true` and are rejected in the production profile.
JWT/claim validation uses pinned `golang-jwt/jwt/v5` and `keyfunc/v3` modules;
authorization and customer-scope decisions remain in GTB middleware.

## Core Banking (B01)

`make db-migrate db-seed` applies Core Banking migrations 0002–0003 and the
`local-b01-v1` fixtures: Maya (SGD 1,000.00), Daniel (SGD 200.00), outgoing
clearing, settlement/nostro and incoming suspense accounts, all opened through one
balanced journal. Seeds are idempotent and never reset data.

```sh
make corebanking-integration   # PostgreSQL: scenarios A–E, immutability, concurrency, lost reply, 409
make corebanking-smoke         # service on loopback 18084: auth, reserve/replay/409, lookup, release, statement
```

Both build database URLs from `.env` without printing them. Integration tests
create their own random-ID accounts beside the fixtures. The smoke adds the
`core-banking-service` audience mapper through the Keycloak admin API when a
realm imported before B01 lacks it; fresh realms get it from the fixture. Run
`core-banking-service` with `GTB_DATABASE_URL`, `GTB_AUTH_AUDIENCE=core-banking-service`
and the usual `GTB_AUTH_*` settings (default address `127.0.0.1:8084`). The port
contract is `contracts/openapi/v1/core-banking.json`; see ADR 0008.

Other feature tables and business seeds remain feature-phase work.
