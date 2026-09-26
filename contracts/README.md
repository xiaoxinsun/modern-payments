# Shared boundary contracts

Versioned shared interfaces required by foundation Section 8. Conventions and
semantics are in [ADR 0002](../spec/adr/0002-shared-contracts-and-api-conventions.md).

| Directory | Current contents |
| --- | --- |
| `common/v1/` | JSON Schema definitions for money, identity, sequences, timestamps and errors |
| `openapi/v1/` | OpenAPI reusable schemas, headers, pagination, security and errors; `core-banking.json` port (B01); `payment.json` commands (B02) |
| `events/v1/` | JSON Schema for domain/posting event envelopes and the `CoreBankingPostingCommitted` and `PaymentReceived` payloads |
| `simulators/v1/` | JSON Schema for the generic simulator message envelope |
| `corebanking/v1/` | Core Banking port semantics summary |
| `examples/` | Synthetic contract fixtures checked by Go tests |

Run `make test` from the repository root to validate schemas, references, examples
and rejected inputs. Schema `$id` values are stable identifiers under
`https://gtb.local/contracts/`, not hosted endpoints; tests load local files and
make no network requests. `shared.json` keeps empty paths; each feature adds its own OpenAPI document
(B01: `core-banking.json`; B02: `payment.json`). Event/simulator payloads are generic objects until those phases add
their own payload schemas and focused contract tests.

Contracts describe boundary data, not internal database rows. Keep payment,
Core Banking and simulator ownership separate. A wire-schema version does not
establish a rail's business outcome or authorize access.
