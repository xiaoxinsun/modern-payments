#!/usr/bin/env python3
"""Reconcile realm additions made after a volume's first import; never prints credentials.

Keycloak imports gtb-realm.json only when the realm does not exist, so objects
added by later phases are created here idempotently. Existing objects,
passwords and secrets are left unchanged.
"""
import json
from pathlib import Path
import sys
from urllib import error, parse, request

ROOT = Path(__file__).resolve().parents[2]
ENV_FILE = ROOT / "infra" / "compose" / ".env"
REALM_FILE = ROOT / "infra" / "compose" / "oidc" / "gtb-realm.json"
BASE = "http://127.0.0.1:8080"
ADMIN = BASE + "/admin/realms/gtb"
# B02: payment-service identity and the demo customer's Core Banking customer ID.
CLIENTS = ("payment-service",)
USER_CUSTOMER_IDS = {"customer.demo": ("20000000-0000-4000-8000-000000000001",)}


def credentials():
    values = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    return values


def call(method, url, token, payload=None):
    headers = {"Authorization": f"Bearer {token}"}
    body = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(payload).encode("utf-8")
    try:
        with request.urlopen(request.Request(url, data=body, headers=headers, method=method), timeout=5) as response:
            text = response.read().decode("utf-8")
            return response.status, json.loads(text) if text else None
    except error.HTTPError as failure:
        raise RuntimeError(f"{method} {url.replace(BASE, '')} returned HTTP {failure.code}") from None


def admin_token(password):
    outgoing = request.Request(
        BASE + "/realms/master/protocol/openid-connect/token",
        data=parse.urlencode({
            "grant_type": "password", "client_id": "admin-cli", "username": "gtb_admin", "password": password,
        }).encode("ascii"),
        headers={"Content-Type": "application/x-www-form-urlencoded"}, method="POST",
    )
    with request.urlopen(outgoing, timeout=5) as response:
        return json.load(response)["access_token"]


def substitute(value, values):
    if isinstance(value, str) and value.startswith("${") and value.endswith("}"):
        name = value[2:-1]
        if not values.get(name):
            raise RuntimeError(f"missing local credential {name}; run make infra-init")
        return values[name]
    return value


def ensure_client(token, realm, client_id, values):
    _, found = call("GET", ADMIN + "/clients?clientId=" + parse.quote(client_id), token)
    if len(found) > 1:
        raise RuntimeError(f"OIDC client {client_id} is ambiguous")
    if not found:
        representation = next(item for item in realm["clients"] if item["clientId"] == client_id)
        representation = {key: substitute(value, values) for key, value in representation.items()}
        call("POST", ADMIN + "/clients", token, representation)
        _, found = call("GET", ADMIN + "/clients?clientId=" + parse.quote(client_id), token)
        print(f"Created OIDC client {client_id}")
    else:
        print(f"OIDC client {client_id} already present")
    account = next((user for user in realm["users"] if user.get("serviceAccountClientId") == client_id), None)
    if not account:
        return
    _, service_user = call("GET", f"{ADMIN}/clients/{found[0]['id']}/service-account-user", token)
    _, assigned = call("GET", f"{ADMIN}/users/{service_user['id']}/role-mappings/realm", token)
    missing = set(account.get("realmRoles", [])) - {role["name"] for role in assigned}
    roles = [call("GET", f"{ADMIN}/roles/{parse.quote(name)}", token)[1] for name in sorted(missing)]
    if roles:
        call("POST", f"{ADMIN}/users/{service_user['id']}/role-mappings/realm", token, roles)
        print(f"Granted {client_id} service account realm roles: {', '.join(sorted(missing))}")


def ensure_customer_ids_profile(token):
    # Keycloak 26 hides and ignores unmanaged attributes in the admin API. Declare
    # customer_ids as an admin-only managed attribute so scope can be maintained.
    _, profile = call("GET", ADMIN + "/users/profile", token)
    if any(attribute["name"] == "customer_ids" for attribute in profile.get("attributes", [])):
        return
    profile.setdefault("attributes", []).append({
        "name": "customer_ids", "displayName": "Customer IDs", "multivalued": True,
        "permissions": {"view": ["admin"], "edit": ["admin"]},
    })
    call("PUT", ADMIN + "/users/profile", token, profile)
    print("Declared admin-only customer_ids user profile attribute")


def ensure_customer_ids(token, username, customer_ids):
    _, users = call("GET", ADMIN + "/users?exact=true&username=" + parse.quote(username), token)
    if len(users) != 1:
        raise RuntimeError(f"OIDC user {username} is missing or ambiguous")
    _, user = call("GET", f"{ADMIN}/users/{users[0]['id']}", token)
    attributes = user.get("attributes") or {}
    current = attributes.get("customer_ids", [])
    wanted = current + [customer for customer in customer_ids if customer not in current]
    if wanted == current:
        print(f"OIDC user {username} customer scope already present")
        return
    attributes["customer_ids"] = wanted
    user["attributes"] = attributes
    call("PUT", f"{ADMIN}/users/{user['id']}", token, user)
    print(f"Added customer scope to OIDC user {username}")


def main():
    values = credentials()
    if not values.get("OIDC_ADMIN_PASSWORD"):
        raise RuntimeError("missing local credentials; run make infra-init")
    realm = json.loads(REALM_FILE.read_text(encoding="utf-8"))
    token = admin_token(values["OIDC_ADMIN_PASSWORD"])
    for client_id in CLIENTS:
        ensure_client(token, realm, client_id, values)
    ensure_customer_ids_profile(token)
    for username, customer_ids in USER_CUSTOMER_IDS.items():
        ensure_customer_ids(token, username, customer_ids)
    print("OIDC realm additions reconciled")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)
