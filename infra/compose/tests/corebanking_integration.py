#!/usr/bin/env python3
"""Run B01 PostgreSQL integration tests without printing local credentials."""
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[3]
ENV_FILE = ROOT / "infra" / "compose" / ".env"


def main():
    credentials = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            credentials[key] = value
    password = credentials.get("COREBANKING_DB_PASSWORD")
    if not password:
        raise RuntimeError("missing Core Banking database credential; run make infra-init")
    environment = os.environ.copy()
    environment["GTB_COREBANKING_TEST_DATABASE_URL"] = (
        "postgresql://corebanking:" + quote(password, safe="") +
        "@127.0.0.1:5432/corebanking?sslmode=disable"
    )
    subprocess.run(
        ["go", "test", "-count=1", "-run", "^TestPostgres", "./internal/corebanking"],
        cwd=ROOT / "backend", env=environment, check=True,
    )
    print("B01 Core Banking PostgreSQL integration checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
