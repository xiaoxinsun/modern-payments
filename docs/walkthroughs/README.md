# GTB walkthroughs

These are hands-on tours of what each implementation phase built: which services
to run, which API calls to make, and which database rows those calls change. They
are for people who want to understand the code by using it. For operating the
local infrastructure itself (volumes, resets, health checks), see the
[local development runbook](../runbooks/local-development.md).

| Walkthrough | Phase | You will see |
| --- | --- | --- |
| [Core Banking accounts, holds and postings](b01-core-banking.md) | B01 | Booked vs. available balance, holds, balanced postings, reversals, operation-ID idempotency |
| [Payment commands and durable receipts](b02-payment-commands.md) | B02 | Submitting a payment, idempotent replay and 409, customer scoping, the rows one receipt writes |

Read this page first. It covers the setup that every walkthrough shares.

**Conventions.**
- Each walkthrough is named `<phase>-<topic>.md` and assumes the setup below.
- A phase with nothing new to run adds a short section to the walkthrough it
  extends, rather than getting a page of its own.
- Every command in a walkthrough was run against the local stack when it was
  written. Outputs differ only in generated IDs and timestamps.

## 0. Starting from nothing

The walkthroughs rebuild the *local environment* from the code in your checkout.
They don't restore code, and they don't restore specific data you created
earlier.

- **Code** comes from git. Anything uncommitted exists only in your working tree,
  so commit or stash before deleting a checkout.
- **Local data** (databases, Keycloak, Kafka, Temporal) lives in Docker volumes.
  To wipe it deliberately, run `make infra-reset CONFIRM=delete-gtb-data`. Every
  payment and posting you created is lost, and the fixtures come back from the
  seeds.
- **Credentials** live in `infra/compose/.env`, which a reset keeps. If you delete
  it too, `make infra-init` generates new ones. Only do that together with a
  volume reset, because existing volumes keep the old database passwords.
- On a fresh volume Keycloak imports the realm, including the B02 additions, and
  `make oidc-sync` then has nothing to add except the `customer_ids` profile
  declaration.

Then run section 1 below, and follow the walkthroughs in phase order.

## 1. Start the infrastructure (I02)

Install Docker (with Compose v2.20+), Python 3, Go 1.27.1, `curl` and `jq`
(1.7 or newer). From the repository root:

```sh
make infra-init infra-up                  # credentials in infra/compose/.env, start containers
make db-provision db-migrate db-seed      # databases, tables, synthetic fixtures
make oidc-sync                            # add post-import realm objects (safe to rerun)
make deps build                           # download pinned Go modules; compile all nine binaries into .build/bin/
make check                                # optional: the full Go test gate
```

These containers are infrastructure only: PostgreSQL, Keycloak (OIDC), Kafka,
Temporal and its UI. The GTB application services are ordinary Go binaries that
you start yourself (section 3). Data persists in Docker volumes across restarts.
Never run `make infra-reset` unless you intend to delete everything.

## 2. Shell helpers

The generated passwords and client secrets live only in the git-ignored
`infra/compose/.env`. Don't print, paste or commit them. Paste these helpers
into each terminal you use, from the repository root. They read values
without echoing them:

```sh
# Read one value from the local credential file.
gtbenv() { sed -n "s/^$1=//p" infra/compose/.env; }

# psql inside the PostgreSQL container, as the local administrator.
gtbsql() { docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml exec postgres psql -U gtb_admin "$@"; }

TOKEN_URL=http://localhost:8080/realms/gtb/protocol/openid-connect/token

# Access token for a demo user (password grant through the local gtb-cli client).
user_token() {
  curl -s "$TOKEN_URL" -d grant_type=password -d client_id=gtb-cli \
    -d "username=$1" --data-urlencode "password=$(gtbenv "$2")" | jq -r .access_token
}

# Access token for a service identity (client-credentials grant).
service_token() {
  curl -s "$TOKEN_URL" -d grant_type=client_credentials -d "client_id=$1" \
    --data-urlencode "client_secret=$(gtbenv "$2")" | jq -r .access_token
}

# Show a token's claims (never paste real tokens into websites).
claims() { jq -R 'split(".")[1] | @base64d | fromjson' <<< "$1"; }

newid() { uuidgen | tr '[:upper:]' '[:lower:]'; }   # GTB IDs are lowercase UUIDv4
```

Access tokens expire after **5 minutes**. When a call suddenly returns `401`,
fetch a fresh token.

## 3. Run the application services

Use one terminal per service, each with the helpers loaded. Logs are JSON lines
on stdout; pipe them through `jq -c .` if you want them easier to read. Stop a
service with Ctrl-C, which triggers a graceful shutdown.

**Terminal A: Core Banking (B01), port 8084**

```sh
GTB_DATABASE_URL="postgresql://corebanking:$(gtbenv COREBANKING_DB_PASSWORD)@127.0.0.1:5432/corebanking?sslmode=disable" \
GTB_AUTH_AUDIENCE=core-banking-service \
.build/bin/core-banking-service
```

**Terminal B: payment API (B02), port 8082**

```sh
GTB_DATABASE_URL="postgresql://payment:$(gtbenv PAYMENT_DB_PASSWORD)@127.0.0.1:5432/payment?sslmode=disable" \
GTB_SERVICE_CLIENT_SECRET="$(gtbenv PAYMENT_SERVICE_OIDC_SECRET)" \
.build/bin/payment-api
```

Check both are up:

```sh
curl -s localhost:8084/health/ready    # {"status":"ready"}
curl -s localhost:8082/health/ready    # {"status":"ready"}
```

The defaults are loopback addresses and the issuer `http://localhost:8080/realms/gtb`.
For payment-api, the Core Banking URL defaults to `http://127.0.0.1:8084` and the
service client to `payment-service`. The generated passwords are hex, so they are
safe inside URLs. Each service logs its non-secret configuration at startup.

The other seven binaries (`payment-worker`, `query-api`, simulators and so on)
are placeholders until their phases arrive.

## 4. Identities

| Identity | How to get a token | Roles and scope | Useful for |
| --- | --- | --- | --- |
| `customer.demo` | `user_token customer.demo GTB_CUSTOMER_PASSWORD` | `customer`; `customer_ids` includes Maya's customer `20000000-0000-4000-8000-000000000001` | Submitting and reading payments |
| `operations.demo` | `user_token operations.demo GTB_OPERATIONS_PASSWORD` | `ops_viewer`, `investigator` | Reading any receipt; cannot submit |
| `compliance.demo` | `user_token compliance.demo GTB_COMPLIANCE_PASSWORD` | `compliance_reviewer` | Showing a role that's denied payment reads |
| `auditor.demo` | `user_token auditor.demo GTB_AUDITOR_PASSWORD` | `auditor` | Read-only access |
| `payment-service` | `service_token payment-service PAYMENT_SERVICE_OIDC_SECRET` | `internal_service`, audience `core-banking-service` | Calling Core Banking directly |

Tokens name their intended recipient in the `aud` claim: user tokens are for
`payment-api`, and `payment-service` tokens are for `core-banking-service`. Each
service rejects tokens meant for another one. Try `claims "$(user_token
customer.demo GTB_CUSTOMER_PASSWORD)"` to see `aud`, `realm_access.roles` and
`customer_ids`.

Fixture accounts, opened through one balanced journal:

| Account | ID | Kind | Opening balance |
| --- | --- | --- | --- |
| Maya | `10000000-0000-4000-8000-000000000001` | customer deposit | SGD 1,000.00 |
| Daniel | `10000000-0000-4000-8000-000000000002` | customer deposit (customer `…0002`) | SGD 200.00 |
| Outgoing clearing | `10000000-0000-4000-8000-000000000003` | system | 0 |
| Settlement/nostro | `10000000-0000-4000-8000-000000000004` | system | SGD 1,200.00 |
| Incoming suspense | `10000000-0000-4000-8000-000000000005` | system | 0 |

Money is always in **minor units as a decimal string**: `{"amountMinor":"2500","currency":"SGD"}`
means SGD 25.00. GTB never uses floating point for money.

## 5. Postman or Bruno instead of curl

Create an environment with `paymentApi = http://localhost:8082` and
`coreBanking = http://localhost:8084`, then configure OAuth 2.0 authorization:

- **Customer or operator requests:** use the *Password Credentials* grant, with
  access token URL `http://localhost:8080/realms/gtb/protocol/openid-connect/token`,
  client ID `gtb-cli`, no client secret, and the demo username and its password
  from `.env`.
- **Core Banking requests:** use the *Client Credentials* grant, with client ID
  `payment-service` and the `PAYMENT_SERVICE_OIDC_SECRET` value.

Store secrets as secret or local-only variables, never in a shared or committed
collection. Add an `Idempotency-Key` header to every `POST /api/v1/payments`.

## 6. Look at the database

```sh
gtbsql -d payment        # interactive psql; \dt app.* lists tables, \d app.payments describes one
gtbsql -d corebanking
```

Every service owns its own database (`payment`, `corebanking`, `projections`,
`simulators`), and no service reads another's tables. For a GUI client
(DBeaver, DataGrip), see the [database guide](../../backend/migrations/README.md).
The `gtb_admin` superuser can read everything, but database triggers still block
it from changing append-only history.

The integration tests (`make *-integration`) write their own rows beside the
fixtures, such as random test accounts and payments for random customers. Filter
by the IDs you created rather than expecting empty tables.

## 7. Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `401` on a call that worked earlier | The token expired after 5 minutes; fetch a new one |
| `403` for `customer.demo` on Maya's account | The realm predates B02; run `make oidc-sync`, then fetch a new token |
| payment-api exits with `GTB_SERVICE_CLIENT_ID and GTB_SERVICE_CLIENT_SECRET are required` | `PAYMENT_SERVICE_OIDC_SECRET` is missing; run `make infra-init infra-up oidc-sync` |
| `503 DEPENDENCY_UNAVAILABLE` on a new payment | Core Banking (terminal A) is not running |
| `bind: address already in use` | An earlier run is still alive; check with `lsof -nP -iTCP:8082 -sTCP:LISTEN` |
| `/health/ready` returns `not_ready` | PostgreSQL or Keycloak is down; check `make infra-status` |

## Automated equivalents

Each walkthrough has a scripted counterpart that runs on throwaway ports:
`make auth-smoke` (I03), `make corebanking-integration corebanking-smoke` (B01) and
`make payment-integration payment-smoke` (B02). Reading those scripts in
`infra/compose/tests/` is another good way to learn the APIs.
