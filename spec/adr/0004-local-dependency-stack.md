# ADR 0004: Persistent local dependency stack

Date: 2026-09-26. Status: accepted for the first I02 scope item.

Follow-up: [ADR 0005](0005-domain-provisioning-and-migrations.md) supplies
application database ownership, migrations, seeds and verification commands.

## Decision

Use Compose project `gtb` with exact image release tags: PostgreSQL
`16.15-bookworm`, Temporal server/admin-tools `1.31.0`, Temporal UI `2.49.1`,
Apache Kafka `4.1.2`, and Keycloak `26.7.4`. Tags pin releases, not immutable
registry digests. Runtime compatibility evidence belongs in the I02 checkpoint;
this selection alone does not prove that the stack is healthy.

PostgreSQL 16 follows Temporal's current PostgreSQL Compose baseline. Use the
server plus separate admin-tools schema job, rather than the retired auto-setup
image. Temporal owns two databases and a non-superuser role; Keycloak owns a
separate database and role on the same server. PUBLIC loses database access.
Later I02 work adds application database owners without exposing the server's
administrator credentials to applications. No shared application tables or
cross-domain transactions are introduced.

Persist PostgreSQL and Kafka in named volumes. Temporal and Keycloak persist
through their databases; the UI and setup jobs have no independent durable
state. Kafka has one combined KRaft controller/broker and separate advertised
addresses for host and Compose clients. No ZooKeeper is needed. Topics are
created explicitly by their owning phase.

Generate credentials locally, never overwrite them on ordinary startup, and
publish services on loopback. Keycloak development mode and unauthenticated
Temporal/Kafka are local-only infrastructure choices. I03 supplies the GTB OIDC
realm, service identities and application authorization. Ordinary down/up retains
volumes; deletion is a separately documented explicit command. This does not
change the foundation's production exclusions or security requirements.

## Sources and follow-up

Reviewed primary upstream documentation for this implementation:

- [Temporal PostgreSQL Compose example](https://github.com/temporalio/samples-server/blob/main/compose/docker-compose-postgres.yml)
  and [version selections](https://github.com/temporalio/samples-server/blob/main/compose/.env).
- [PostgreSQL official image](https://hub.docker.com/_/postgres) for release tags,
  first-volume initialization and the PostgreSQL 16 data mount path.
- [Kafka 4.1 Docker guide](https://kafka.apache.org/41/getting-started/docker/)
  and [single-node configuration](https://github.com/apache/kafka/blob/trunk/docker/examples/docker-compose-files/single-node/plaintext/docker-compose.yml).
- [Keycloak container guide](https://www.keycloak.org/server/containers),
  [release example](https://www.keycloak.org/getting-started/getting-started-docker)
  and [readiness probes](https://www.keycloak.org/observability/health).

The remaining I02 work must verify application ownership, migration/seed
commands and restart persistence before phase completion. Container upgrades
must preserve data and include schema compatibility checks; deleting volumes
is not an upgrade strategy.
