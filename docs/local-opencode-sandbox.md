# Local OpenCode sandbox

Local OpenCode execution can use [Anthropic Sandbox Runtime](https://github.com/anthropic-experimental/sandbox-runtime), `srt`, as its operating-system sandbox. The engine is tested against the exact npm package:

```text
@anthropic-ai/sandbox-runtime@0.0.70
```

The runtime verifies the package name and version from the installed package metadata before it starts a command. It refuses a different version, a wrapper that cannot be tied to the package, a missing platform dependency, or an unsupported platform. It never falls back to direct execution.

## Install the pinned executable

Install `srt` into a dedicated tool directory rather than globally:

```bash
tool_root="$HOME/.local/share/prow-ai-dashboard/srt-0.0.70"
npm install --prefix "$tool_root" --save-exact @anthropic-ai/sandbox-runtime@0.0.70
export SRT_BIN="$tool_root/node_modules/.bin/srt"
```

The package requires Node.js 20.11 or newer. Platform dependencies are:

- macOS: `rg` (`ripgrep`).
- Linux: `bwrap` (`bubblewrap`), `socat`, and `rg`.

Linux also needs capability-bearing unprivileged user namespaces. If the host security policy prevents `bubblewrap` or the bundled seccomp helper from creating them, the runtime fails closed.

## Enforced boundary

The local sandbox wraps the complete OpenCode process tree. Its generated policy:

- denies host filesystem reads by default, then allows the temporary checkout, temporary OpenCode home, dedicated temporary directory, the selected executable, certificate paths, and minimal platform runtime paths;
- permits writes only in the temporary checkout, home, and temporary directory;
- denies writes to the checkout's `.git` directory and to `srt`'s shared default temporary path;
- denies outbound network access except reviewed domain and optional port entries;
- denies local port binding, Apple Events, and Unix sockets by default;
- keeps the weaker nested and weaker macOS network modes disabled.

The Git clone occurs before sandbox execution, so a repository token is not sent into OpenCode merely to clone source. The sandbox shares the host kernel. It is not a VM, container, Orka runtime, or Kubernetes SIGs Agent Sandbox.

## Tests

The normal test suite covers policy generation and fail-closed behavior. Hostile integration tests can exercise the installed package on macOS or Linux:

```bash
cd backend
SRT_TEST_BIN="$SRT_BIN" go test ./internal/runtime -run '^TestSRTSandbox(Hostile|Cancellation)Integration$' -count=1
```

A disposable GitHub Copilot smoke test is opt-in and does not print credential values:

```bash
cd backend
SRT_REAL_OPENCODE=1 \
SRT_TEST_BIN="$SRT_BIN" \
SRT_TEST_NETWORK_DOMAINS='models.dev:443,api.githubcopilot.com:443,github.com:443' \
go test ./internal/runtime -run '^TestSRTSandboxRealOpenCode$' -count=1
```

Those three Copilot destinations were observed from `srt` violation logs with an empty allowlist. Other providers and dependency registries require their own explicit reviewed domains.
