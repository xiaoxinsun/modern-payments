# ADR 0005: Local database ownership and migrations

Date: 2026-09-26. Status: accepted for I02.

## Decision

Provision four application databases and same-named owner logins: `payment`,
`corebanking`, `projections`, and `simulators`. This implements foundation
Sections 2 and 4 without changing the separate Temporal/OIDC ownership in
ADR 0004. Application provisioning is an explicit rerunnable command so the
existing I02 volume can be upgraded without reset. It creates missing objects,
rejects conflicting ownership/elevated attributes, and never replaces passwords.
No owner has memberships in another owner. PUBLIC loses database access.

Run local application migrations as the relevant owner, using the pinned
PostgreSQL image's psql client over the authenticated service address. Use
ordinary SQL files with consecutive versions. A Python standard-library wrapper
provides the command entry points; no application SQL driver is needed yet.
Record filename, SHA-256 and application timestamp in `_gtb.schema_migrations`.
Within each database, a transaction advisory lock serializes concurrent runners;
validate all recorded history before applying pending SQL and its ledger entries
atomically. Missing or edited historical files fail. Each domain commits
independently. There are no cross-domain transactions or automatic down scripts.

Keep the initial `app` schemas empty. A stable infrastructure marker in
`_gtb.seed_manifest` demonstrates idempotent seeding and persistence without
deciding future account/payment schemas. Feature-owned SQL must follow ADR 0002
when defining money, identifiers and events. I02 seeds are synthetic metadata,
not simulated balances or proof of a payment flow.

Generated `.env` credentials stay ignored and mode 0600. Initialization appends
missing owner keys without rotating existing keys. Destructive reset requires
its own explicit command and confirmation string. Startup/migrations/seeding
never depend on reset. Database owner credentials are local administration
credentials; future runtime features must add narrower grants when required for
append-only audit or other within-domain invariants.

## Validation and rationale

Live checks require correct-password owner writes, wrong-password rejection,
all cross-domain/admin database write attempts to fail at CONNECT, and absence
of elevated role attributes/memberships. They verify concurrent migration replay,
checksum rejection, rollback of partial DDL and exact seed/ledger preservation
across container recreation. The initial loopback authentication check exposed
the container image's trusted local connection path; tooling now uses the
service address and clean initialization explicitly selects SCRAM for host auth.

This uses PostgreSQL's documented [database privileges](https://www.postgresql.org/docs/16/ddl-priv.html),
[role membership](https://www.postgresql.org/docs/16/role-membership.html),
[psql environment/SQL facilities](https://www.postgresql.org/docs/16/app-psql.html)
and [transaction advisory locks](https://www.postgresql.org/docs/16/functions-admin.html).
Exact local validation evidence is linked from the I02 packet. Hosted CI results
must be observed separately; adding a workflow is not evidence of its execution.
