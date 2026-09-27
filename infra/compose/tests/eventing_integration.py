#!/usr/bin/env python3
"""Run B03 outbox, projection and query integration tests against scratch databases.

Outbox delivery marks rows published and rebuilds switch projection generations,
so these tests never touch the shared payment, corebanking or projections
databases. Each run creates throwaway databases owned by the domain logins,
applies the checked-in migrations, runs the tests and drops the databases.
Credentials are passed through the environment and never printed.
"""
import os
from pathlib import Path
import secrets
import subprocess
import sys
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "infra" / "compose"))
import database as db  # noqa: E402

DOMAINS = ("payment", "corebanking", "projections")


def url(role, password, database):
    return f"postgresql://{role}:{quote(password, safe='')}@127.0.0.1:5432/{database}?sslmode=disable"


def main():
    credentials = db.credentials()
    suffix = secrets.token_hex(4)
    names = {domain: f"{domain}_it_{suffix}" for domain in DOMAINS}
    try:
        for domain, name in names.items():
            db.psql("gtb_admin", "postgres", "POSTGRES_ADMIN_PASSWORD",
                    f"CREATE DATABASE {name} OWNER {domain};\nREVOKE ALL ON DATABASE {name} FROM PUBLIC;\n")
            db.psql(domain, name, f"{domain.upper()}_DB_PASSWORD", "REVOKE ALL ON SCHEMA public FROM PUBLIC;")
            db.psql(domain, name, f"{domain.upper()}_DB_PASSWORD",
                    db.migration_sql(ROOT / "backend" / "migrations" / domain))
        db.psql("gtb_admin", "postgres", "POSTGRES_ADMIN_PASSWORD",
                f"GRANT CONNECT ON DATABASE {names['projections']} TO {db.READER};")
        print(f"Scratch databases migrated: {', '.join(names.values())}", flush=True)
        environment = os.environ.copy()
        environment["GTB_PAYMENT_SCRATCH_DATABASE_URL"] = url("payment", credentials["PAYMENT_DB_PASSWORD"], names["payment"])
        environment["GTB_COREBANKING_SCRATCH_DATABASE_URL"] = url("corebanking", credentials["COREBANKING_DB_PASSWORD"], names["corebanking"])
        environment["GTB_PROJECTIONS_SCRATCH_DATABASE_URL"] = url("projections", credentials["PROJECTIONS_DB_PASSWORD"], names["projections"])
        environment["GTB_PROJECTIONS_SCRATCH_READER_URL"] = url(db.READER, credentials["QUERY_READER_DB_PASSWORD"], names["projections"])
        flags = ["-race"] if os.environ.get("GTB_RACE") == "1" else []
        subprocess.run(["go", "test", "-count=1", "-v", *flags, "-run", "^TestScratch", "./internal/..."],
                       cwd=ROOT / "backend", env=environment, check=True)
        print("B03 eventing integration checks passed")
    finally:
        for name in names.values():
            db.psql("gtb_admin", "postgres", "POSTGRES_ADMIN_PASSWORD", f"DROP DATABASE IF EXISTS {name} WITH (FORCE);", check=False)
        print("Scratch databases dropped", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
