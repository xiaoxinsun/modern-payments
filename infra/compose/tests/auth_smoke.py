#!/usr/bin/env python3
"""Exercise the I03 OIDC realm and payment-api runtime without printing secrets."""
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from urllib import error, parse, request

ROOT = Path(__file__).resolve().parents[3]
ENV_FILE = ROOT / "infra" / "compose" / ".env"
BINARY = ROOT / ".build" / "bin" / "payment-api"
OIDC_TOKEN_URL = "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/token"
API = "http://127.0.0.1:18082"


def load_credentials():
    values = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    return values


def http(method, url, token=None, headers=None):
    outgoing = dict(headers or {})
    if token:
        outgoing["Authorization"] = f"Bearer {token}"
    body = b"{}" if method == "POST" else None
    if body is not None:
        outgoing["Content-Type"] = "application/json"
    outgoing_request = request.Request(url, data=body, headers=outgoing, method=method)
    try:
        with request.urlopen(outgoing_request, timeout=4) as response:
            return response.status, dict(response.headers), response.read().decode("utf-8")
    except error.HTTPError as failure:
        return failure.code, dict(failure.headers), failure.read().decode("utf-8")
    except error.URLError:
        return 0, {}, ""


def token(form):
    outgoing = request.Request(
        OIDC_TOKEN_URL,
        data=parse.urlencode(form).encode("ascii"),
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )
    try:
        with request.urlopen(outgoing, timeout=5) as response:
            payload = json.load(response)
    except error.HTTPError as failure:
        detail = failure.read().decode("utf-8")
        raise RuntimeError(f"OIDC token request for {form.get('client_id')} failed with HTTP {failure.code}: {detail}") from None
    return payload["access_token"]


def user_token(username, password):
    return token({
        "grant_type": "password", "client_id": "gtb-cli",
        "username": username, "password": password,
    })


def service_token(client_id, secret):
    return token({
        "grant_type": "client_credentials", "client_id": client_id,
        "client_secret": secret,
    })


def claims(access_token):
    payload = access_token.split(".")[1]
    payload += "=" * (-len(payload) % 4)
    return json.loads(base64.urlsafe_b64decode(payload))


def expect(name, actual, wanted):
    if actual != wanted:
        raise AssertionError(f"{name}: got HTTP {actual}, want {wanted}")
    print(f"PASS {name}: HTTP {actual}")


def main():
    credentials = load_credentials()
    required = [
        "GTB_CUSTOMER_PASSWORD", "GTB_OPERATIONS_PASSWORD",
        "GTB_OPERATIONS_APPROVER_PASSWORD", "GTB_COMPLIANCE_PASSWORD",
        "GTB_COMPLIANCE_APPROVER_PASSWORD", "GTB_AUDITOR_PASSWORD",
        "GTB_PLATFORM_ADMIN_PASSWORD",
        "RAIL_SIMULATOR_OIDC_SECRET", "CORE_BANKING_OIDC_SECRET",
        "EXPIRED_TOKEN_TEST_OIDC_SECRET",
    ]
    missing = [name for name in required if not credentials.get(name)]
    if missing:
        raise RuntimeError("missing local credentials; run make infra-init")
    if not BINARY.exists():
        raise RuntimeError("payment-api binary missing; run make build")

    service_environment = os.environ.copy()
    service_environment.update({
        "GTB_HTTP_ADDRESS": "127.0.0.1:18082",
        "GTB_PROFILE": "local",
        "GTB_AUTH_ISSUER": "http://localhost:8080/realms/gtb",
        "GTB_AUTH_JWKS_URL": "http://127.0.0.1:8080/realms/gtb/protocol/openid-connect/certs",
        "GTB_AUTH_AUDIENCE": "payment-api",
    })
    log_file = tempfile.NamedTemporaryFile(prefix="gtb-i03-payment-api-", suffix=".jsonl", delete=False)
    log_path = Path(log_file.name)
    process = subprocess.Popen([str(BINARY)], cwd=ROOT, env=service_environment,
                               stdout=log_file, stderr=subprocess.STDOUT)
    try:
        for _ in range(80):
            if process.poll() is not None:
                raise RuntimeError(f"payment-api exited early with {process.returncode}")
            status, _, _ = http("GET", API + "/health/ready")
            if status == 200:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("payment-api did not become ready")

        expect("liveness", http("GET", API + "/health/live")[0], 200)
        expect("readiness against OIDC", http("GET", API + "/health/ready")[0], 200)
        expect("missing token denied", http("GET", API + "/api/v1/runtime/customers/cust-demo-001")[0], 401)
        expect("invalid token denied", http("GET", API + "/api/v1/runtime/customers/cust-demo-001", "invalid.token.value")[0], 401)

        customer = user_token("customer.demo", credentials["GTB_CUSTOMER_PASSWORD"])
        operations = user_token("operations.demo", credentials["GTB_OPERATIONS_PASSWORD"])
        operations_approver = user_token("operations.approver", credentials["GTB_OPERATIONS_APPROVER_PASSWORD"])
        compliance = user_token("compliance.demo", credentials["GTB_COMPLIANCE_PASSWORD"])
        compliance_approver = user_token("compliance.approver", credentials["GTB_COMPLIANCE_APPROVER_PASSWORD"])
        auditor = user_token("auditor.demo", credentials["GTB_AUDITOR_PASSWORD"])
        platform_admin = user_token("platform.demo", credentials["GTB_PLATFORM_ADMIN_PASSWORD"])
        integration = service_token("rail-simulator", credentials["RAIL_SIMULATOR_OIDC_SECRET"])
        internal = service_token("core-banking-service", credentials["CORE_BANKING_OIDC_SECRET"])

        expect("customer own scope allowed", http("GET", API + "/api/v1/runtime/customers/cust-demo-001", customer)[0], 200)
        expect("customer other scope denied", http("GET", API + "/api/v1/runtime/customers/cust-other", customer)[0], 403)
        expect("wrong operator role denied", http("GET", API + "/api/v1/runtime/customers/cust-demo-001", operations)[0], 403)
        expect("customer denied operations", http("GET", API + "/api/v1/runtime/operations", customer)[0], 403)
        expect("operations role allowed", http("GET", API + "/api/v1/runtime/operations", operations)[0], 200)
        expect("operations approver allowed", http("GET", API + "/api/v1/runtime/operations", operations_approver)[0], 200)
        expect("compliance role allowed", http("GET", API + "/api/v1/runtime/operations", compliance)[0], 200)
        expect("compliance approver allowed", http("GET", API + "/api/v1/runtime/operations", compliance_approver)[0], 200)
        expect("auditor role allowed", http("GET", API + "/api/v1/runtime/operations", auditor)[0], 200)
        expect("integration service allowed", http("POST", API + "/integrations/v1/runtime/messages", integration)[0], 200)
        expect("wrong service role denied", http("POST", API + "/integrations/v1/runtime/messages", internal)[0], 403)
        expect("dev controls disabled by default", http("GET", API + "/dev/v1/runtime/config", platform_admin)[0], 404)

        role_sets = {
            "operations maker": set(claims(operations).get("realm_access", {}).get("roles", [])),
            "operations approver": set(claims(operations_approver).get("realm_access", {}).get("roles", [])),
            "compliance reviewer": set(claims(compliance).get("realm_access", {}).get("roles", [])),
            "compliance approver": set(claims(compliance_approver).get("realm_access", {}).get("roles", [])),
        }
        if "approver" in role_sets["operations maker"] or "investigator" in role_sets["operations approver"]:
            raise AssertionError("operations maker/checker roles are not separated")
        if "compliance_approver" in role_sets["compliance reviewer"] or "compliance_reviewer" in role_sets["compliance approver"]:
            raise AssertionError("compliance reviewer/approver roles are not separated")
        print("PASS operations and compliance maker/checker roles are separate")

        bad_value = "not-a-safe-id"
        bad_status, bad_headers, bad_body = http("GET", API + "/health/live", headers={"X-Correlation-ID": bad_value})
        expect("invalid correlation ID denied", bad_status, 400)
        if bad_headers.get("X-Correlation-Id") == bad_value or bad_value in bad_body:
            raise AssertionError("unchecked correlation ID was reflected")
        print("PASS invalid correlation ID replaced safely")

        expired = service_token("expired-token-test", credentials["EXPIRED_TOKEN_TEST_OIDC_SECRET"])
        expiry = int(claims(expired)["exp"])
        time.sleep(max(0, expiry - int(time.time()) + 1))
        expect("expired token denied", http("POST", API + "/integrations/v1/runtime/messages", expired)[0], 401)

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

    log_lines = log_path.read_text(encoding="utf-8").splitlines()
    for line in log_lines:
        json.loads(line)
    for name, secret in credentials.items():
        if ("PASSWORD" in name or "SECRET" in name) and secret and any(secret in line for line in log_lines):
            raise AssertionError(f"credential value leaked in service log for {name}")
    if not any('"msg":"shutdown complete"' in line for line in log_lines):
        raise AssertionError("shutdown completion was not logged")
    log_path.unlink()
    print("PASS structured JSON logs parsed and contained no local credential values")
    print("I03 authentication/runtime smoke checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
