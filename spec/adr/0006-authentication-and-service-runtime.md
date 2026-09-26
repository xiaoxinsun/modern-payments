# ADR 0006: Local OIDC authorization and service runtime

Date: 2026-09-26. Status: accepted for I03.

## Context

Foundation Sections 4, 8, 9 and 11 require authenticated customer, operator,
compliance and integration boundaries, service identities, correlation, readiness,
redacted telemetry and graceful shutdown. Feature phases need these controls before
they expose business endpoints. Production identity federation remains outside the
local reference application's scope.

## Decision

Use the pinned I02 Keycloak service as the local OIDC issuer. Import a versioned
`gtb` realm fixture containing least-privilege realm roles, separate synthetic
maker/checker users, a customer scope attribute, a public local smoke client and
confidential service-account clients. Client secrets and human passwords are
generated once in ignored mode-0600 `.env` storage and substituted by Keycloak at
import; no credential value is committed. Tokens for APIs must have a valid RS256
signature from the configured JWKS, exact issuer, intended audience, subject and
active time window. Authentication never treats a request customer ID as scope.

Implement JWT validation with the maintained `golang-jwt/jwt/v5` package and use
`MicahParks/keyfunc/v3` for remote JWKS retrieval, caching and key rotation. Allow
only RS256 signature-use keys and require a key ID, the exact issuer, intended
audience, expiry, subject and active time window. Reusable GTB middleware enforces bearer authentication,
any-of role policy and explicit customer scope. It maps realm roles and client
roles for the intended audience only, so an unrelated resource role cannot grant
authority. `/integrations/v1` uses confidential service identity plus the
`integration_ingress` role; a client secret alone is not authorization. The
selection and wider dependency review are recorded in [ADR 0007](0007-maintained-libraries-and-project-owned-code.md).

Use a reusable runtime package for environment configuration, loopback-safe local
defaults, correlation IDs, stable error envelopes, public liveness/readiness,
structured JSON access logs with key-based redaction, panic containment and bounded
HTTP shutdown. Readiness stops accepting work before shutdown and checks OIDC
signing-key availability. Development controls require both the local profile and
an explicit enable flag; the sample service does not enable them by default.

## Consequences

I03 exposes only protected runtime demonstration routes, not payment behavior. Each
feature phase must still declare and test its own roles, customer/account/team scope
and command-time reauthorization. Keycloak `start-dev`, password-grant smoke users
and static local confidential clients are development fixtures, not production
identity architecture. Production deployments must provide a trusted issuer,
managed client credentials, TLS and their own network binding configuration.
