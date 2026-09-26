# ADR 0003: Global toolchain and baseline checks

Date: 2026-09-26. Status: accepted; supersedes ADR 0001's Go version and
test-dependency deferral only.

The user installed Go through Homebrew. Observed `/opt/homebrew/bin/go` reports
`go1.27.1 darwin/arm64`, with GOROOT under `/opt/homebrew/Cellar/go/1.27.1/libexec`.
Pin Go **1.27.1** in `backend/go.mod`, use `GOTOOLCHAIN=local`, and have
`make toolchain` enforce that exact version. CI reads the same module version.
No temporary Go installation participates in final I01 validation; temporary
build/module caches only accommodate the workspace sandbox.

At I01 completion, runtime code needed only the standard library. I03 subsequently
adopted maintained JWT/JWKS and UUID packages; [ADR 0007](0007-maintained-libraries-and-project-owned-code.md)
records the current build-versus-adopt policy and supersedes the earlier runtime
dependency assumption. The initial direct test dependency remains
`github.com/santhosh-tekuri/jsonschema/v6 v6.0.2`; its transitive dependency is
`golang.org/x/text v0.14.0`. Dependencies are pinned in `go.mod`/`go.sum`.
The validator's own tests additionally pin `github.com/dlclark/regexp2 v1.11.0`
in the resolved checksum graph; this is not a GTB runtime dependency.
The validator supports draft 2020-12 and format assertions; its
[upstream documentation](https://github.com/santhosh-tekuri/jsonschema/tree/v6.0.2)
describes those capabilities. It is not imported by domain or runtime code.
Compatibility is verified with Go 1.27.1 by `make check`.

Do not add unused Temporal, PostgreSQL or Kafka modules to fake dependency
completeness. Their owning infrastructure/runtime phases select and pin versions
alongside actual code and compatibility tests. I02 owns container version pins.
This keeps I01 outside running infrastructure and business features.

Root Make targets provide formatting, formatting checks, module consistency and
checksum verification, vetting, unit/contract/architecture tests, race tests and
all nine builds. `make check` is the local and CI gate. `make deps` downloads the
pinned dependencies. Builds go to ignored `.build/bin/`. The architecture test
checks transitive domain/value dependencies against HTTP/network, SQL, Temporal
and outer packages; future domain packages are included automatically.

Baseline CI uses Ubuntu 24.04, `actions/checkout@v4.2.2` and
`actions/setup-go@v6.0.0`, read-only repository permissions and a 15-minute timeout.
The [setup-go action](https://github.com/actions/setup-go/tree/v6.0.0) reads
`backend/go.mod` and keys the module cache with `backend/go.sum`.
Local passing checks are evidence for code validation; they are not a claim
that a hosted Actions run has occurred. Integration/recovery tests are added
with their owning phases once executable infrastructure exists.
