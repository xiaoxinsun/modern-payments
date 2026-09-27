#!/usr/bin/env python3
"""Live I02 checks. Requires provisioned/migrated/seeded local dependencies."""
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location("database", Path(__file__).resolve().parents[1] / "database.py")
db = importlib.util.module_from_spec(spec)
spec.loader.exec_module(db)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def verify():
    services = subprocess.run(db.COMPOSE + ["ps", "--all", "--format", "json"],
                              check=True, capture_output=True, text=True).stdout
    rows = [json.loads(line) for line in services.splitlines() if line.strip()]
    status = {row["Service"]: row for row in rows}
    for service in ("postgres", "temporal", "temporal-ui", "kafka", "oidc"):
        require(status[service]["Health"] == "healthy", f"{service} not healthy")
    for service in ("temporal-schema", "temporal-namespace"):
        require(status[service]["State"] == "exited" and status[service]["ExitCode"] == 0,
                f"{service} did not complete successfully")
    print("PASS dependency health and setup jobs", flush=True)

    # Save complete metadata, including timestamps, to detect unintended rewrites.
    def snapshot(domain):
        result = db.domain_sql(domain, """
SELECT json_build_object('migrations', (SELECT json_agg(m ORDER BY name) FROM _gtb.schema_migrations m),
                         'seed', (SELECT json_agg(s ORDER BY dataset) FROM _gtb.seed_manifest s));
""").stdout.strip()
        value = json.loads(result)
        require(len(value["migrations"]) >= 1, f"Missing migrations in {domain}")
        require(any(s["dataset"] == "local-i02-v1" and s["domain"] == domain for s in value["seed"]),
                f"Missing or incorrect seed in {domain}")
        return result

    before = {domain: snapshot(domain) for domain in db.DOMAINS}
    db.provision()
    # Concurrent invocations must serialize and remain idempotent.
    with ThreadPoolExecutor(max_workers=2) as pool:
        list(pool.map(lambda _: db.migrate(), range(2)))
    db.seed()
    require(before == {domain: snapshot(domain) for domain in db.DOMAINS},
            "Provision/migration/seed replay changed existing metadata")
    print("PASS concurrent migrations and repeat provisioning/seeding preserve data", flush=True)

    owners = {domain: ([domain], f"{domain.upper()}_DB_PASSWORD") for domain in db.DOMAINS}
    owners.update(temporal=(["temporal", "temporal_visibility"], "TEMPORAL_DB_PASSWORD"),
                  oidc=(["oidc"], "OIDC_DB_PASSWORD"))
    databases = list(db.DOMAINS) + ["temporal", "temporal_visibility", "oidc", "postgres"]
    denied = 0
    for role, (allowed, key) in owners.items():
        # A successful own-database write establishes credentials and writability.
        for database in allowed:
            db.psql(role, database, key, "BEGIN; CREATE TABLE public.i02_write_probe(id integer); ROLLBACK;")
        for database in databases:
            if database in allowed:
                continue
            result = db.psql(role, database, key, "CREATE TABLE public.i02_forbidden_write(id integer);", check=False)
            require(result.returncode != 0 and "permission denied for database" in result.stderr,
                    f"Expected CONNECT denial for {role} -> {database}; got {result.stderr}")
            denied += 1
        result = db.psql(role, allowed[0], key, "SELECT 1;",
                        {"PGPASSWORD": "intentionally-wrong-i02-password"}, check=False)
        require(result.returncode != 0 and "password authentication failed" in result.stderr,
                f"Password authentication not enforced for {role}")
        attributes = db.psql(role, allowed[0], key, """
SELECT NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
       AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member = current_user::regrole)
FROM pg_roles WHERE rolname = current_user;
""").stdout.strip()
        require(attributes == "t", f"Elevated role or membership for {role}")
    print(f"PASS {denied} cross-domain/admin database write attempts denied; six owner logins authenticate without elevated roles", flush=True)

    # B03 CQRS read credentials: query_reader reads query models only.
    reader, key = db.READER, "QUERY_READER_DB_PASSWORD"
    require(db.psql(reader, db.READER_DATABASE, key, "SELECT count(*) >= 1 FROM app.projection_generations WHERE status = 'ACTIVE';").stdout.strip() == "t",
            "query_reader cannot read the active projection generation")
    reader_denied = 0
    for statement in ("INSERT INTO app.consumer_status VALUES ('i02-probe', now(), NULL);",
                      "SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE; DELETE FROM app.payment_summary;",
                      "SELECT record_value FROM app.repair_queue;", "SELECT envelope FROM app.pending_events;",
                      "SELECT * FROM app.inbox;", "CREATE TABLE app.reader_probe(id integer);"):
        result = db.psql(reader, db.READER_DATABASE, key, statement, check=False)
        require(result.returncode != 0 and ("permission denied" in result.stderr or "read-only transaction" in result.stderr),
                f"Expected query_reader denial for {statement!r}; got {result.stderr}")
        reader_denied += 1
    for database in databases:
        if database == db.READER_DATABASE:
            continue
        result = db.psql(reader, database, key, "SELECT 1;", check=False)
        require(result.returncode != 0 and "permission denied for database" in result.stderr,
                f"Expected CONNECT denial for {reader} -> {database}; got {result.stderr}")
        reader_denied += 1
    attributes = db.psql(reader, db.READER_DATABASE, key, """
SELECT NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
       AND NOT EXISTS (SELECT FROM pg_auth_members WHERE member = current_user::regrole)
FROM pg_roles WHERE rolname = current_user;
""").stdout.strip()
    require(attributes == "t", f"Elevated role or membership for {reader}")
    print(f"PASS query_reader reads query models; {reader_denied} writes, private columns and other databases denied", flush=True)

    # A changed historical file must fail before executing any DDL.
    original = db.ROOT / "backend/migrations/payment"
    with tempfile.TemporaryDirectory(prefix="gtb-i02-migrations-") as temporary:
        directory = Path(temporary)
        for path in original.glob("*.sql"):
            (directory / path.name).write_bytes(path.read_bytes())
        first = directory / "0001_local_scaffold.sql"
        first.write_text(first.read_text() + "\n-- edited historical migration\n")
        result = db.domain_sql("payment", db.migration_sql(directory), check=False)
        require(result.returncode != 0 and "checksum changed" in result.stderr, "Changed migration accepted")
        first.write_bytes((original / first.name).read_bytes())
        version = len(list(directory.glob("*.sql"))) + 1
        (directory / f"{version:04d}_failure_probe.sql").write_text(
            "CREATE TABLE app.i02_rollback_probe(id integer);\n"
            "DO $$ BEGIN RAISE EXCEPTION 'i02 injected migration failure'; END $$;\n")
        result = db.domain_sql("payment", db.migration_sql(directory), check=False)
        require(result.returncode != 0 and "i02 injected migration failure" in result.stderr,
                "Injected failing migration succeeded")
        require(db.domain_sql("payment", "SELECT to_regclass('app.i02_rollback_probe') IS NULL;").stdout.strip() == "t",
                "Failed migration left partial DDL")
    require(snapshot("payment") == before["payment"], "Failed migration changed ledger or seeds")
    print("PASS migration checksum rejection and atomic rollback", flush=True)
    print("PASS I02 live verification (restart persistence is a separate down/up comparison)", flush=True)


if __name__ == "__main__":
    verify()
