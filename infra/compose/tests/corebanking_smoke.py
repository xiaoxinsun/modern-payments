#!/usr/bin/env python3
"""Exercise the B01 authenticated HTTP port without printing credentials."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from urllib import error, parse, request
import uuid

ROOT = Path(__file__).resolve().parents[3]
ENV_FILE = ROOT / "infra" / "compose" / ".env"
BINARY = ROOT / ".build" / "bin" / "core-banking-service"
OIDC_TOKEN_URL = "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/token"
API = "http://127.0.0.1:18084"
MAYA_ACCOUNT = "10000000-0000-4000-8000-000000000001"


def credentials():
    values = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    return values


def service_token(client_id, secret):
    outgoing = request.Request(
        OIDC_TOKEN_URL,
        data=parse.urlencode({
            "grant_type": "client_credentials", "client_id": client_id,
            "client_secret": secret,
        }).encode("ascii"),
        headers={"Content-Type": "application/x-www-form-urlencoded"}, method="POST",
    )
    with request.urlopen(outgoing, timeout=5) as response:
        return json.load(response)["access_token"]


def ensure_core_banking_audience(admin_password):
    outgoing = request.Request(
        "http://127.0.0.1:8080/realms/master/protocol/openid-connect/token",
        data=parse.urlencode({
            "grant_type": "password", "client_id": "admin-cli",
            "username": "gtb_admin", "password": admin_password,
        }).encode("ascii"),
        headers={"Content-Type": "application/x-www-form-urlencoded"}, method="POST",
    )
    with request.urlopen(outgoing, timeout=5) as response:
        admin_token = json.load(response)["access_token"]
    headers = {"Authorization": f"Bearer {admin_token}"}
    clients_request = request.Request(
        "http://127.0.0.1:8080/admin/realms/gtb/clients?clientId=core-banking-service",
        headers=headers,
    )
    with request.urlopen(clients_request, timeout=5) as response:
        clients = json.load(response)
    if len(clients) != 1:
        raise RuntimeError("Core Banking OIDC client is missing or ambiguous")
    mappers_url = f"http://127.0.0.1:8080/admin/realms/gtb/clients/{clients[0]['id']}/protocol-mappers/models"
    with request.urlopen(request.Request(mappers_url, headers=headers), timeout=5) as response:
        mappers = json.load(response)
    if any(item.get("name") == "core-banking-service-audience" for item in mappers):
        return
    mapper = {
        "name": "core-banking-service-audience",
        "protocol": "openid-connect",
        "protocolMapper": "oidc-audience-mapper",
        "config": {
            "included.client.audience": "core-banking-service",
            "access.token.claim": "true",
        },
    }
    create_request = request.Request(
        mappers_url, data=json.dumps(mapper).encode("utf-8"),
        headers={**headers, "Content-Type": "application/json"}, method="POST",
    )
    with request.urlopen(create_request, timeout=5) as response:
        if response.status != 201:
            raise RuntimeError(f"Core Banking audience mapper returned HTTP {response.status}")


def http(method, path, token=None, payload=None):
    headers = {"X-Correlation-ID": str(uuid.uuid4())}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    body = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    outgoing = request.Request(API + path, data=body, headers=headers, method=method)
    try:
        with request.urlopen(outgoing, timeout=5) as response:
            text = response.read().decode("utf-8")
            return response.status, json.loads(text) if text else None
    except error.HTTPError as failure:
        text = failure.read().decode("utf-8")
        return failure.code, json.loads(text) if text else None
    except error.URLError:
        return 0, None


def expect(name, actual, wanted):
    if actual != wanted:
        raise AssertionError(f"{name}: got HTTP {actual}, want {wanted}")
    print(f"PASS {name}: HTTP {actual}")


def expect_denied(name, actual):
    if actual not in (401, 403):
        raise AssertionError(f"{name}: got HTTP {actual}, want 401 or 403")
    print(f"PASS {name}: HTTP {actual}")


def main():
    values = credentials()
    required = ["COREBANKING_DB_PASSWORD", "OIDC_ADMIN_PASSWORD", "RAIL_SIMULATOR_OIDC_SECRET", "CORE_BANKING_OIDC_SECRET"]
    missing = [name for name in required if not values.get(name)]
    if missing:
        raise RuntimeError("missing local credentials; run make infra-init")
    if not BINARY.exists():
        raise RuntimeError("Core Banking binary missing; run make build")

    environment = os.environ.copy()
    environment.update({
        "GTB_HTTP_ADDRESS": "127.0.0.1:18084",
        "GTB_PROFILE": "local",
        "GTB_AUTH_ISSUER": "http://localhost:8080/realms/gtb",
        "GTB_AUTH_JWKS_URL": "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/certs",
        "GTB_AUTH_AUDIENCE": "core-banking-service",
        "GTB_DATABASE_URL": "postgresql://corebanking:" + parse.quote(values["COREBANKING_DB_PASSWORD"], safe="") + "@127.0.0.1:5432/corebanking?sslmode=disable",
    })
    log_file = tempfile.NamedTemporaryFile(prefix="gtb-b01-core-banking-", suffix=".jsonl", delete=False)
    log_path = Path(log_file.name)
    process = subprocess.Popen([str(BINARY)], cwd=ROOT, env=environment,
                               stdout=log_file, stderr=subprocess.STDOUT)
    try:
        for _ in range(80):
            if process.poll() is not None:
                raise RuntimeError(f"Core Banking service exited early with {process.returncode}")
            if http("GET", "/health/ready")[0] == 200:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("Core Banking service did not become ready")

        ensure_core_banking_audience(values["OIDC_ADMIN_PASSWORD"])
        internal = service_token("core-banking-service", values["CORE_BANKING_OIDC_SECRET"])
        wrong_service = service_token("rail-simulator", values["RAIL_SIMULATOR_OIDC_SECRET"])
        account_path = f"/integrations/v1/core-banking/accounts/{MAYA_ACCOUNT}"
        expect("missing token denied", http("GET", account_path)[0], 401)
        expect_denied("wrong service identity denied", http("GET", account_path, wrong_service)[0])
        status, account = http("GET", account_path, internal)
        expect("internal-service account lookup", status, 200)
        if account["bookedMinor"] != account["availableMinor"]:
            raise AssertionError("seeded Maya account unexpectedly has an active hold")

        operation_id = str(uuid.uuid4())
        reserve = {
            "operationId": operation_id, "accountId": MAYA_ACCOUNT,
            "amount": {"amountMinor": "1", "currency": "SGD"},
        }
        status, first = http("POST", "/integrations/v1/core-banking/holds", internal, reserve)
        expect("reserve available funds", status, 200)
        status, second = http("POST", "/integrations/v1/core-banking/holds", internal, reserve)
        expect("duplicate reserve replays", status, 200)
        if first["holdId"] != second["holdId"]:
            raise AssertionError("duplicate reserve created a second hold")
        conflicting = dict(reserve)
        conflicting["amount"] = {"amountMinor": "2", "currency": "SGD"}
        expect("conflicting operation rejected", http("POST", "/integrations/v1/core-banking/holds", internal, conflicting)[0], 409)
        expect("operation lookup after simulated lost reply",
               http("GET", f"/integrations/v1/core-banking/operations/{operation_id}", internal)[0], 200)
        release = {"operationId": str(uuid.uuid4())}
        expect("explicit hold release", http("POST", f"/integrations/v1/core-banking/holds/{first['holdId']}/releases", internal, release)[0], 200)
        expect("statement export", http("GET", account_path + "/statement?limit=10", internal)[0], 200)

        process.send_signal(signal.SIGTERM)
        process.wait(timeout=10)
        if process.returncode != 0:
            raise AssertionError(f"graceful shutdown returned {process.returncode}")
        print("PASS SIGTERM graceful shutdown: exit 0")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        log_file.close()

    lines = log_path.read_text(encoding="utf-8").splitlines()
    for line in lines:
        json.loads(line)
    for name, secret in values.items():
        if ("PASSWORD" in name or "SECRET" in name) and secret and any(secret in line for line in lines):
            raise AssertionError(f"credential value leaked in service log for {name}")
    if not any('"msg":"shutdown complete"' in line for line in lines):
        raise AssertionError("shutdown completion was not logged")
    log_path.unlink()
    print("PASS structured logs parsed and contained no local credential values")
    print("B01 Core Banking authenticated API smoke checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
