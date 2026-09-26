#!/usr/bin/env python3
"""Exercise the B02 payment command API end to end without printing credentials."""
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
sys.path.insert(0, str(ROOT / "infra" / "compose"))
import oidc_sync  # noqa: E402

ENV_FILE = ROOT / "infra" / "compose" / ".env"
BIN = ROOT / ".build" / "bin"
OIDC_TOKEN_URL = "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/token"
PAYMENT_API = "http://127.0.0.1:18082"
CORE_BANKING = "http://127.0.0.1:18084"
MAYA_CUSTOMER = "20000000-0000-4000-8000-000000000001"
DANIEL_CUSTOMER = "20000000-0000-4000-8000-000000000002"
MAYA_ACCOUNT = "10000000-0000-4000-8000-000000000001"
DANIEL_ACCOUNT = "10000000-0000-4000-8000-000000000002"


def credentials():
    values = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    return values


def token(form):
    outgoing = request.Request(
        OIDC_TOKEN_URL, data=parse.urlencode(form).encode("ascii"),
        headers={"Content-Type": "application/x-www-form-urlencoded"}, method="POST",
    )
    with request.urlopen(outgoing, timeout=5) as response:
        return json.load(response)["access_token"]


def user_token(username, password):
    return token({"grant_type": "password", "client_id": "gtb-cli", "username": username, "password": password})


def http(method, url, bearer=None, payload=None, headers=None):
    outgoing = {"X-Correlation-ID": str(uuid.uuid4()), **(headers or {})}
    if bearer:
        outgoing["Authorization"] = f"Bearer {bearer}"
    body = None
    if payload is not None:
        outgoing["Content-Type"] = "application/json"
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    try:
        with request.urlopen(request.Request(url, data=body, headers=outgoing, method=method), timeout=8) as response:
            text = response.read().decode("utf-8")
            return response.status, dict(response.headers), json.loads(text) if text else None
    except error.HTTPError as failure:
        text = failure.read().decode("utf-8")
        return failure.code, dict(failure.headers), json.loads(text) if text else None
    except error.URLError:
        return 0, {}, None


def expect(name, actual, wanted):
    if actual != wanted:
        raise AssertionError(f"{name}: got {actual}, want {wanted}")
    print(f"PASS {name}: {actual}")


class Service:
    def __init__(self, name, binary, environment, health_url):
        self.name, self.binary, self.environment, self.health_url = name, binary, environment, health_url
        self.log = tempfile.NamedTemporaryFile(prefix=f"gtb-b02-{name}-", suffix=".jsonl", delete=False)
        self.process = None

    def start(self):
        self.process = subprocess.Popen([str(self.binary)], cwd=ROOT, env=self.environment,
                                        stdout=self.log, stderr=subprocess.STDOUT)
        for _ in range(80):
            if self.process.poll() is not None:
                raise RuntimeError(f"{self.name} exited early with {self.process.returncode}")
            if http("GET", self.health_url)[0] == 200:
                return
            time.sleep(0.25)
        raise RuntimeError(f"{self.name} did not become ready")

    def stop(self):
        self.process.send_signal(signal.SIGTERM)
        self.process.wait(timeout=15)
        if self.process.returncode != 0:
            raise AssertionError(f"{self.name} graceful shutdown returned {self.process.returncode}")

    def kill(self):
        if self.process and self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)


def base_environment(address, audience, database, password):
    environment = os.environ.copy()
    environment.update({
        "GTB_HTTP_ADDRESS": address, "GTB_PROFILE": "local",
        "GTB_AUTH_ISSUER": "http://localhost:8080/realms/gtb",
        "GTB_AUTH_JWKS_URL": "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/certs",
        "GTB_AUTH_AUDIENCE": audience,
        "GTB_DATABASE_URL": f"postgresql://{database}:" + parse.quote(password, safe="") + f"@127.0.0.1:5432/{database}?sslmode=disable",
    })
    return environment


def payment_request(amount="100", debtor=MAYA_ACCOUNT, customer=MAYA_CUSTOMER, creditor=DANIEL_ACCOUNT):
    return {
        "customerId": customer, "product": "BOOK_TRANSFER", "debtorAccountId": debtor,
        "amount": {"amountMinor": amount, "currency": "SGD"},
        "creditor": {"name": "Daniel", "accountId": creditor}, "reference": "B02 smoke",
    }


def main():
    values = credentials()
    required = ["PAYMENT_DB_PASSWORD", "COREBANKING_DB_PASSWORD", "PAYMENT_SERVICE_OIDC_SECRET",
                "GTB_CUSTOMER_PASSWORD", "GTB_OPERATIONS_PASSWORD", "GTB_COMPLIANCE_PASSWORD", "OIDC_ADMIN_PASSWORD"]
    if any(not values.get(name) for name in required):
        raise RuntimeError("missing local credentials; run make infra-init")
    for binary in ("payment-api", "core-banking-service"):
        if not (BIN / binary).exists():
            raise RuntimeError(f"{binary} binary missing; run make build")
    oidc_sync.main()

    core = Service("core-banking", BIN / "core-banking-service",
                   base_environment("127.0.0.1:18084", "core-banking-service", "corebanking", values["COREBANKING_DB_PASSWORD"]),
                   CORE_BANKING + "/health/ready")
    payment_environment = base_environment("127.0.0.1:18082", "payment-api", "payment", values["PAYMENT_DB_PASSWORD"])
    payment_environment.update({
        "GTB_COREBANKING_URL": CORE_BANKING, "GTB_SERVICE_TOKEN_URL": OIDC_TOKEN_URL,
        "GTB_SERVICE_CLIENT_SECRET": values["PAYMENT_SERVICE_OIDC_SECRET"],
    })
    payment = Service("payment-api", BIN / "payment-api", payment_environment, PAYMENT_API + "/health/ready")
    services = [core, payment]
    try:
        core.start()
        payment.start()
        customer = user_token("customer.demo", values["GTB_CUSTOMER_PASSWORD"])
        operations = user_token("operations.demo", values["GTB_OPERATIONS_PASSWORD"])
        compliance = user_token("compliance.demo", values["GTB_COMPLIANCE_PASSWORD"])
        submit = PAYMENT_API + "/api/v1/payments"
        key = {"Idempotency-Key": "b02-smoke-" + str(uuid.uuid4())}

        expect("missing token denied", http("POST", submit, None, payment_request(), key)[0], 401)
        expect("operator cannot submit", http("POST", submit, operations, payment_request(), key)[0], 403)
        expect("foreign debtor account denied",
               http("POST", submit, customer, payment_request(debtor=DANIEL_ACCOUNT, creditor=MAYA_ACCOUNT), {"Idempotency-Key": "b02-foreign-" + str(uuid.uuid4())})[0], 403)
        expect("customer outside token scope denied",
               http("POST", submit, customer, payment_request(customer=DANIEL_CUSTOMER, debtor=DANIEL_ACCOUNT, creditor=MAYA_ACCOUNT), {"Idempotency-Key": "b02-scope-" + str(uuid.uuid4())})[0], 403)
        expect("contract rejects non-canonical amount", http("POST", submit, customer, payment_request(amount="0100"), key)[0], 400)

        status, headers, first = http("POST", submit, customer, payment_request(), key)
        expect("durable receipt", status, 202)
        expect("new receipt not replayed", headers.get("Idempotent-Replayed"), "false")
        expect("receipt is received, not accepted", (first["lifecycleState"], first["executionOutcome"], first["receiptVersion"]), ("RECEIVED", "NOT_SUBMITTED", "1"))
        expect("workflow dispatch pending", first["dispatch"]["status"], "PENDING")
        status, headers, second = http("POST", submit, customer, payment_request(), key)
        expect("same key and payload replays", (status, headers.get("Idempotent-Replayed")), (202, "true"))
        expect("replay returns one payment ID", (second["paymentId"], second["commandId"]), (first["paymentId"], first["commandId"]))
        status, _, conflict = http("POST", submit, customer, payment_request(amount="101"), key)
        expect("conflicting reuse", (status, conflict["error"]["code"]), (409, "CONFLICT"))

        receipt = f"{PAYMENT_API}/api/v1/payments/{first['paymentId']}/receipt"
        expect("customer reads own receipt", http("GET", receipt, customer)[0], 200)
        expect("operations reads receipt", http("GET", receipt, operations)[0], 200)
        expect("compliance role denied receipt", http("GET", receipt, compliance)[0], 403)
        status, _, command = http("GET", f"{PAYMENT_API}/api/v1/commands/{first['commandId']}", customer)
        expect("command receipt", (status, command["status"], command["paymentId"]), (200, "COMMITTED", first["paymentId"]))

        payment.stop()
        print("PASS payment-api SIGTERM graceful shutdown: exit 0")
        payment.start()
        status, _, record = http("GET", receipt, customer)
        expect("receipt survives restart with pending dispatch",
               (status, record["dispatch"]["status"], record["lifecycleState"], record["version"]), (200, "PENDING", "RECEIVED", "1"))
        status, _, replay = http("POST", submit, customer, payment_request(), key)
        expect("replay after restart", (status, replay["paymentId"]), (202, first["paymentId"]))

        core.stop()
        status, _, unavailable = http("POST", submit, customer, payment_request(), {"Idempotency-Key": "b02-outage-" + str(uuid.uuid4())})
        expect("new instruction during Core Banking outage", (status, unavailable["error"]["code"], unavailable["error"]["retryable"]), (503, "DEPENDENCY_UNAVAILABLE", True))
        status, headers, replay = http("POST", submit, customer, payment_request(), key)
        expect("replay needs no Core Banking", (status, headers.get("Idempotent-Replayed"), replay["paymentId"]), (202, "true", first["paymentId"]))
        payment.stop()
        print("PASS final graceful shutdown: exit 0")
    finally:
        for service in services:
            service.kill()
            service.log.close()

    for service in services:
        path = Path(service.log.name)
        lines = path.read_text(encoding="utf-8").splitlines()
        for line in lines:
            json.loads(line)
        for name, secret in values.items():
            if ("PASSWORD" in name or "SECRET" in name) and secret and any(secret in line for line in lines):
                raise AssertionError(f"credential value leaked in {service.name} log for {name}")
        path.unlink()
    print("PASS structured logs parsed and contained no local credential values")
    print(f"B02 payment API smoke checks passed (payment {first['paymentId']})")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
