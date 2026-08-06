# Local OpenCode sandbox

Local OpenCode callers use the `srt` backend by default. This includes onboarding prompt authoring and local fix generation. The backend uses [Anthropic Sandbox Runtime](https://github.com/anthropic-experimental/sandbox-runtime), `srt`, and is tested against release `v0.0.70` at commit `44ab607c46f20381aeaf3e22ca0e0151d4c6b29c`.

```text
@anthropic-ai/sandbox-runtime@0.0.70
```

The runtime verifies the package name and version from the installed package metadata before it starts a command. It refuses a different version, a wrapper that cannot be tied to the package, a missing platform dependency, or an unsupported platform. It never falls back to direct execution.

## Install the pinned executable

The `0.0.70` npm package is not published in every registry. The repository
installer downloads the pinned upstream source archive, verifies its SHA-256,
builds the package from its lock file, installs only the runtime dependencies,
and records the verified source identity in `INSTALL_PROVENANCE`.
Install it into a dedicated tool directory rather than globally:

```bash
tool_root="$HOME/.local/share/prow-ai-dashboard/srt-0.0.70"
./hack/install-srt.sh "$tool_root"
export SRT_BIN="$tool_root/node_modules/.bin/srt"
```

`NewLocalAgent` reads `SRT_BIN` through `NewSRTSandboxFromEnv`. Missing, mismatched, or unusable `srt` fails closed. There is no direct-execution fallback.

The package requires Node.js 20.11 or newer. Platform dependencies are:

- macOS: `bash` and `rg` (`ripgrep`).
- Linux: `bash`, `bwrap` (`bubblewrap`), `socat`, and `rg`.

Linux also needs capability-bearing unprivileged user namespaces. If the host security policy prevents `bubblewrap` or the bundled seccomp helper from creating them, the runtime fails closed.

## Enforced boundary

The local sandbox wraps the complete OpenCode process tree. Its generated policy:

- denies host filesystem reads by default, then allows the temporary checkout, temporary OpenCode home, dedicated temporary directory, the selected executable, certificate paths, and minimal platform runtime paths;
- permits writes only in the temporary checkout, home, and temporary directory;
- denies writes to the checkout's `.git` directory and to `srt`'s shared default temporary path;
- denies outbound network access except reviewed domain and optional port entries;
- denies host-visible local port binding, Apple Events, and Unix sockets by default;
- keeps the weaker nested and weaker macOS network modes disabled.

On Linux, a process can bind only inside `srt`'s isolated network namespace, so it cannot expose a host port. Requests to enable local binding are rejected because v0.0.70 does not implement that option on Linux.

On macOS, the runner tracks descendant process identities and kills recorded descendants on cancellation and normal command exit, including children that create another session. macOS does not provide a kernel process container here, so this lifecycle cleanup has a narrow polling race for a process that daemonizes and reparents before it is observed. Any surviving descendant remains subject to the inherited `srt` filesystem and network policy.

The immutable system and toolchain paths in the policy are readable, not hidden. On macOS this includes `/System`, `/etc`, and the specific Homebrew Bash, readline, ncurses, and gettext directories required by the tested shell. Do not store project credentials or service secrets in those system paths.

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

## Network policy by caller

Onboarding with `github-copilot/*` always includes the three domains observed in the controlled smoke test above. Repeated `--prompt-network-domain=<domain[:port]>` flags can add other explicitly reviewed destinations. Other native OpenCode providers require at least one such flag.

Local fix generation automatically allows the configured AI endpoint host. Add `ai.fix_prs.agent_runtime.network_domains` only for dependency registries or other reviewed destinations required by Bash-enabled fix and test commands. Entries are domains with an optional port, not URLs. Credential-bearing URLs are rejected.

OpenCode `allow_bash` controls only the OpenCode Bash tool. It never broadens the filesystem, socket, local binding, environment, or network sandbox policy.
