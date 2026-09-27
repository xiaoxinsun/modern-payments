# Local GTB dependencies

Run commands from the repository root. Install Docker with Compose v2.20 or
newer and Python 3; start the Docker daemon first. This stack runs infrastructure
dependencies; application services are started separately. `payment-api` durably
receives customer payment instructions (B02), `core-banking-service` serves the
Core Banking port (B01), and `payment-worker`, `event-projector` and `query-api`
deliver events and serve payment queries (B03); other business services remain
feature scaffolds.
For hands-on tours of the running services, see the
[walkthroughs](../walkthroughs/README.md).

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
| `projections` | `PROJECTIONS_DB_PASSWORD` | Query projections (written by event-projector) |
| `query_reader` (connects to `projections` only) | `QUERY_READER_DB_PASSWORD` | Read-only query-api login (B03) |
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

Since B02, `payment-api` also needs `GTB_DATABASE_URL` and its service identity
(below); the smoke supplies both from `.env`. The check starts `payment-api` on loopback port 18082, verifies public liveness and
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

## Payment commands (B02)

`make db-migrate` applies payment migration 0002 (payments, immutable
instructions, version history, idempotency records, commands, audit, domain
events/outbox and workflow dispatch). B02 adds a confidential `payment-service`
OIDC client (role `internal_service`, audience `core-banking-service`) and gives
`customer.demo` Maya's Core Banking customer ID
`20000000-0000-4000-8000-000000000001` alongside `cust-demo-001`. A realm imported
before B02 lacks both; reconcile it without resetting data:

```sh
make infra-init                # adds PAYMENT_SERVICE_OIDC_SECRET to .env if missing
make infra-up                  # recreates Keycloak with the new secret variable
make oidc-sync                 # idempotent: creates payment-service, declares customer_ids, adds scope
make payment-integration       # PostgreSQL: atomic receipt, replay/409, injected failures, races, guards
make payment-smoke             # payment-api 18082 + core-banking-service 18084 end to end
```

`oidc-sync` declares `customer_ids` as an admin-only managed user-profile
attribute because Keycloak 26 hides unmanaged attributes from the admin API.
The smoke checks role/scope denial, 202 receipts with pending dispatch, replay
and 409, receipt/command reads, a payment-api restart and a Core Banking outage
(new instructions get a retryable 503; replays still succeed).

Run `payment-api` with `GTB_DATABASE_URL`, `GTB_AUTH_AUDIENCE=payment-api`, the
usual `GTB_AUTH_*` settings, `GTB_SERVICE_CLIENT_SECRET` (the
`PAYMENT_SERVICE_OIDC_SECRET` value) and optionally `GTB_SERVICE_CLIENT_ID`
(default `payment-service`), `GTB_SERVICE_TOKEN_URL` (default: issuer token
endpoint) and `GTB_COREBANKING_URL` (default `http://127.0.0.1:8084`). The
contract is `contracts/openapi/v1/payment.json`; after changing it run
`make openapi-generate`. See ADR 0009.

## Event delivery and payment queries (B03)

B03 publishes both outboxes to Kafka, projects payment events and serves
queries (ADR 0010). On an existing stack, add the new reader credential, grants
and topics without resetting data:

```sh
make infra-init                # adds QUERY_READER_DB_PASSWORD to .env if missing
make db-provision db-migrate   # creates the query_reader login; payment 0003, projections 0002
make kafka-topics              # gtb.payment.events.v1, gtb.corebanking.postings.v1 (idempotent)
make eventing-integration      # scratch databases: outbox delivery, projector, rebuild, query scope
make eventing-smoke            # all five services on 1808x: outages, redelivery, poison, rebuild
```

`eventing-integration` creates throwaway `*_it_*` databases, migrates them,
runs the `TestScratch*` tests and drops them, so it never touches shared data.
`eventing-smoke` stops and restarts Kafka and the projector, republishes an
event, deletes the projector checkpoint, injects a poison record (then marks it
resolved), rebuilds the projections while payments arrive, and relays a Core
Banking posting and its reversal. Stop any walkthrough `event-projector` first:
both join consumer group `gtb-event-projector`.

| Service | Default address | Database login | Other settings |
| --- | --- | --- | --- |
| `payment-worker` | `127.0.0.1:8086` (health only) | `payment` | `GTB_KAFKA_BROKERS` (default `127.0.0.1:9092`) |
| `core-banking-service` | `127.0.0.1:8084` | `corebanking` | also relays postings; `GTB_KAFKA_BROKERS` |
| `event-projector` | `127.0.0.1:8085` (health only) | `projections` | `GTB_SERVICE_CLIENT_SECRET` (`EVENT_PROJECTOR_OIDC_SECRET`), `GTB_PAYMENT_API_URL` (default `http://127.0.0.1:8082`), `GTB_KAFKA_BROKERS` |
| `query-api` | `127.0.0.1:8083` | `query_reader` | `GTB_PAYMENT_API_URL` (pending receipts), `GTB_PROJECTION_STALE_AFTER` (default `10s`); audience `payment-api` |

Kafka is not a readiness dependency of any service: while it is down, receipts
still commit, events wait in the outboxes (`delivery_attempts`, `last_error`),
and query responses report `projection.freshness: STALE`.

### Runbook: projection lag, poison events and rebuild

1. **Check health.** `GET /api/v1/ops/projections` (operator or
   `platform_admin` token) shows the active generation, per-partition lag,
   broker heartbeat, events buffered behind ordering gaps and open repairs.
2. **STALE.** Kafka or the projector is down. Check `make infra-status`, then the
   projector log. Nothing needs replaying: undelivered events are still in the
   outboxes, and the projector resumes from its database checkpoint.
3. **Buffered gaps** normally clear within seconds (journal catch-up after 5s).
   If they persist, check that payment-api is reachable from the projector.
4. **Open repairs.** Inspect `app.repair_queue` in `projections` as the
   administrator. A poison record never blocks its partition. After fixing the
   cause, rebuild, then mark the entry `RESOLVED`.
5. **Rebuild** (safe with live traffic; the projector must be running so live
   events reach the shadow generation):

   ```sh
   GTB_DATABASE_URL="postgresql://projections:<PROJECTIONS_DB_PASSWORD>@127.0.0.1:5432/projections?sslmode=disable" \
   GTB_SERVICE_CLIENT_SECRET=<EVENT_PROJECTOR_OIDC_SECRET> .build/bin/event-projector rebuild
   ```

   It prints a JSON report and exits 0 only after verification passed and reads
   switched to the new generation. On failure the active generation is
   unchanged, the new one is marked `FAILED`, and `problems` says why. After a
   crash mid-rebuild, run `event-projector rebuild -abandon-building`. The
   walkthrough shows the same command with the credential helpers.

A rebuild re-reads the payment journal through payment-api. It never writes
payment or ledger data, and replayed events never reach effectful consumers.

Other feature tables and business seeds remain feature-phase work.
