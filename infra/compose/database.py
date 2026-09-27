#!/usr/bin/env python3
"""Local provisioning/migration commands; SQL executes through the pinned psql image."""
import argparse
import hashlib
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[2]
DOMAINS = ("payment", "corebanking", "projections", "simulators")
COMPOSE = ["docker", "compose", "--env-file", str(ROOT / "infra/compose/.env"),
           "-f", str(ROOT / "infra/compose/compose.yaml")]


def credentials():
    result = {}
    for line in (ROOT / "infra/compose/.env").read_text().splitlines():
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z_]+", key) or not value or key in result:
            raise ValueError("Invalid or duplicate local credential entry; see infra/compose/.env")
        result[key] = value
    return result


def literal(value):
    return "'" + value.replace("'", "''") + "'"


def psql(role, database, password_key, sql, extra_env=None, check=True):
    secrets = credentials()
    env = dict(os.environ, **secrets, PGPASSWORD=secrets[password_key])
    extra_env = extra_env or {}
    env.update(extra_env)
    # Forward environment by name; passwords never appear in command arguments.
    command = COMPOSE + ["exec", "-T", "-e", "PGPASSWORD"]
    for key in extra_env:
        command += ["-e", key]
    # Use the service address, not container loopback: PostgreSQL's image permits
    # trusted local maintenance connections, which would bypass password checks.
    command += ["postgres", "psql", "-X", "-q", "-A", "-t", "-h", "postgres",
                "-U", role, "-d", database, "-v", "ON_ERROR_STOP=1"]
    result = subprocess.run(command, input=sql, text=True, capture_output=True, env=env)
    if check and result.returncode:
        raise RuntimeError(f"Database operation failed for {role}/{database}: {result.stderr.strip()}")
    return result


def domain_sql(domain, sql, check=True):
    return psql(domain, domain, f"{domain.upper()}_DB_PASSWORD", sql, check=check)


def provision():
    for domain in DOMAINS:
        # Fixed allowlisted identifiers. Lock across role/database creation, which
        # cannot run in a transaction. Replays do not replace existing passwords.
        sql = rf"""
SELECT pg_advisory_lock(710200);
\getenv provision_password GTB_PROVISION_PASSWORD
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L',
              '{domain}', :'provision_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{domain}') \gexec
SELECT 'CREATE DATABASE {domain} OWNER {domain}'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '{domain}') \gexec
DO $$ BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = '{domain}' AND
      (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR NOT rolcanlogin))
     OR EXISTS (SELECT FROM pg_auth_members WHERE member = '{domain}'::regrole)
     OR NOT EXISTS (SELECT FROM pg_database WHERE datname = '{domain}' AND datdba = '{domain}'::regrole)
  THEN RAISE EXCEPTION 'Existing {domain} ownership/role attributes conflict with provisioning'; END IF;
END $$;
REVOKE ALL ON DATABASE {domain} FROM PUBLIC;
ALTER ROLE {domain} IN DATABASE {domain} SET search_path = app, public;
"""
        psql("gtb_admin", "postgres", "POSTGRES_ADMIN_PASSWORD", sql,
             {"GTB_PROVISION_PASSWORD": credentials()[f"{domain.upper()}_DB_PASSWORD"]})
        domain_sql(domain, "REVOKE ALL ON SCHEMA public FROM PUBLIC;")
        print(f"Provisioned {domain}: database owner/login {domain}")
    provision_reader()


# CQRS read credentials: query-api connects as a login that may only read the
# projections database. Table grants come from the projections migrations.
READER, READER_DATABASE = "query_reader", "projections"


def provision_reader():
    sql = rf"""
SELECT pg_advisory_lock(710200);
\getenv provision_password GTB_PROVISION_PASSWORD
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L',
              '{READER}', :'provision_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{READER}') \gexec
DO $$ BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = '{READER}' AND
      (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR NOT rolcanlogin))
     OR EXISTS (SELECT FROM pg_auth_members WHERE member = '{READER}'::regrole)
  THEN RAISE EXCEPTION 'Existing {READER} role attributes conflict with provisioning'; END IF;
END $$;
GRANT CONNECT ON DATABASE {READER_DATABASE} TO {READER};
ALTER ROLE {READER} IN DATABASE {READER_DATABASE} SET search_path = app, public;
ALTER ROLE {READER} SET default_transaction_read_only = on;
"""
    psql("gtb_admin", "postgres", "POSTGRES_ADMIN_PASSWORD", sql,
         {"GTB_PROVISION_PASSWORD": credentials()["QUERY_READER_DB_PASSWORD"]})
    print(f"Provisioned {READER}: read-only login for {READER_DATABASE}")


def migration_sql(directory):
    files = sorted(directory.glob("*.sql"))
    if not files or any(not re.fullmatch(r"[0-9]{4}_[a-z0-9_]+\.sql", p.name) for p in files):
        raise ValueError(f"Invalid or empty migration directory: {directory}")
    if [int(p.name[:4]) for p in files] != list(range(1, len(files) + 1)):
        raise ValueError("Migrations must have consecutive versions starting at 0001")
    manifest = [(p.name, hashlib.sha256(p.read_bytes()).hexdigest(), p.read_text()) for p in files]
    sql = """BEGIN;
SET LOCAL lock_timeout = '30s';
SELECT pg_advisory_xact_lock(710201);
CREATE SCHEMA IF NOT EXISTS _gtb;
CREATE TABLE IF NOT EXISTS _gtb.schema_migrations (
    name text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE TEMP TABLE migration_manifest (name text PRIMARY KEY, sha256 text) ON COMMIT DROP;
"""
    for name, checksum, _ in manifest:
        sql += f"INSERT INTO migration_manifest VALUES ({literal(name)}, {literal(checksum)});\n"
    sql += """DO $$ BEGIN
  IF EXISTS (SELECT FROM _gtb.schema_migrations applied LEFT JOIN migration_manifest source USING (name)
             WHERE source.name IS NULL OR source.sha256 <> applied.sha256)
  THEN RAISE EXCEPTION 'Applied migration is missing or its checksum changed'; END IF;
END $$;
"""
    for name, checksum, content in manifest:
        sql += f"SELECT NOT EXISTS (SELECT FROM _gtb.schema_migrations WHERE name = {literal(name)}) AS apply \\gset\n"
        sql += "\\if :apply\n" + content + "\n"
        sql += f"INSERT INTO _gtb.schema_migrations(name, sha256) VALUES ({literal(name)}, {literal(checksum)});\n"
        sql += "\\endif\n"
    return sql + "COMMIT;\n"


def migrate():
    for domain in DOMAINS:
        domain_sql(domain, migration_sql(ROOT / "backend/migrations" / domain))
        print(f"Migrated {domain}: checksums verified")


def seed():
    sql = (ROOT / "backend/seeds/local.sql").read_text()
    for domain in DOMAINS:
        domain_sql(domain, sql)
        print(f"Seeded {domain}: versioned synthetic fixtures")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("provision", "migrate", "seed"))
    args = parser.parse_args()
    try:
        {"provision": provision, "migrate": migrate, "seed": seed}[args.command]()
    except (ValueError, RuntimeError, KeyError, FileNotFoundError) as error:
        parser.exit(1, str(error) + "\n")


if __name__ == "__main__":
    main()
