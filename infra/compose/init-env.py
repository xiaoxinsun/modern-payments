#!/usr/bin/env python3
"""Create local credentials once; never overwrite credentials for existing volumes."""
import os
import fcntl
from pathlib import Path
import secrets

destination = Path(__file__).resolve().parent / ".env"
descriptor = os.open(destination, os.O_RDWR | os.O_CREAT, 0o600)
with os.fdopen(descriptor, "r+") as output:
    fcntl.flock(output, fcntl.LOCK_EX)
    os.fchmod(output.fileno(), 0o600)
    existing = output.read()
    names = {line.split("=", 1)[0] for line in existing.splitlines() if "=" in line}
    required = ("POSTGRES_ADMIN_PASSWORD", "TEMPORAL_DB_PASSWORD",
                "OIDC_DB_PASSWORD", "OIDC_ADMIN_PASSWORD", "PAYMENT_DB_PASSWORD",
                "COREBANKING_DB_PASSWORD", "PROJECTIONS_DB_PASSWORD",
                "SIMULATORS_DB_PASSWORD")
    missing = [name for name in required if name not in names]
    if missing:
        if not existing:
            output.write("# Local credentials. Keep with the volumes; do not commit.\n")
        elif not existing.endswith("\n"):
            output.write("\n")
        for name in missing:
            output.write(f"{name}={secrets.token_hex(32)}\n")
    print(f"Local credentials ready; added {len(missing)} missing keys; existing values preserved.")
