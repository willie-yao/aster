# Agent Sandbox Fix Runtime Evaluation

This directory contains local-only tooling for the consumer-installed Agent
Sandbox Fix PR runtime. It is not a production installer.

- `Dockerfile` builds the retained deterministic lifecycle fake.
- `fake-gateway.Dockerfile` builds a deterministic credential-free streaming
  OpenAI-compatible gateway.
- `run-kind-evaluation.sh` builds the production OpenCode executor, installs the
  pinned Agent Sandbox v0.5.3 core release in a disposable kind cluster, applies
  Helm-generated RBAC, and creates at most one authorized primary Sandbox.

The harness refuses to overwrite an evidence directory. A primary run also
requires a new `AGENT_SANDBOX_PRIMARY_AUTHORIZATION_ID`; its persistent local
attempt marker prevents accidental reuse of that authorization ID.

Production constructors and production Helm admission require
`RuntimeDefault` AppArmor. Docker Desktop kind does not provide AppArmor, so the
Go test harness uses an internal test-only capability that omits AppArmor from
both the canonical preflight Pod and Sandbox Pod. The production policy is
validated separately with a server-side dry run. The local admission policy
diff changes only the two AppArmor predicates and still denies `Unconfined`.

The final August 8, 2026 structured-command productionization fixture completed
successfully and preserved its evidence alongside every earlier failure and
success. The kind run validates API and lifecycle behavior only. It does not validate AppArmor enforcement, Kata or gVisor isolation, or a
hostile-code security boundary.

## Executor image contract

The engine workflow publishes a generic Linux/amd64 executor containing
OpenCode, Git, and CA certificates. It does not contain Go, `make`, or arbitrary
repository toolchains. The retained README fixture validates patch generation
and `git diff --cached --check`; it is not CAPZ validation.

A consumer that needs repository-specific validators must derive its own image
from the engine executor stage, install only the required tools, and publish it
independently. The derived image must preserve UID/GID 65532, the
`/usr/local/bin/fixexecutor` entrypoint, the credential-free OpenCode config,
read-only-root compatibility, and the same runtime security contract. Deployment
must use the resulting OCI digest. A mutable tag is discovery metadata only.

Validation commands are exact `argv` arrays with explicit timeouts. They run
after the single OpenCode request. A failed or unavailable validator returns a
terminal failure, produces no actionable Fix PR preview, and never triggers a
second model request.
