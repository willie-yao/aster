#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: test-agent-sandbox-fix-image.sh IMAGE EXPECTED_VERSION EXPECTED_COMMIT EXPECTED_IMAGE_TAG}
expected_version=${2:?expected version required}
expected_commit=${3:?expected commit required}
expected_image_tag=${4:?expected image tag required}

tmp=$(mktemp -d)
source_volume="prow-ai-fix-image-fixture-${$}"
created_container=""
cleanup() {
  if [[ -n "$created_container" ]]; then
    docker rm -f "$created_container" >/dev/null 2>&1 || true
  fi
  docker volume rm -f "$source_volume" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

docker image inspect "$image" >"$tmp/image-inspect.json"
docker_security=$(docker info --format '{{json .SecurityOptions}}')
printf '%s\n' "$docker_security" | grep -Fq 'name=seccomp'
python3 - "$tmp/image-inspect.json" "$expected_version" "$expected_commit" "$expected_image_tag" <<'PY'
import json
import sys

path, version, commit, image_tag = sys.argv[1:]
config = json.load(open(path, encoding="utf-8"))[0]["Config"]
assert config["User"] == "65532:65532", config["User"]
assert config["WorkingDir"] == "/workspace", config["WorkingDir"]
assert config["Entrypoint"] == ["/usr/local/bin/fixexecutor"], config["Entrypoint"]
labels = config.get("Labels") or {}
assert labels.get("org.opencontainers.image.source") == "https://github.com/willie-yao/aster", labels
assert labels.get("org.opencontainers.image.title") == "Aster Agent Sandbox Fix Executor", labels
assert labels.get("org.opencontainers.image.url") == "https://github.com/willie-yao/aster", labels
assert labels.get("org.opencontainers.image.version") == version, labels
assert labels.get("org.opencontainers.image.revision") == commit, labels
assert labels.get("io.prow-ai-dashboard.image-tag") == image_tag, labels
environment = "\n".join(config.get("Env") or [])
for forbidden in ("GITHUB_TOKEN=", "KUBERNETES_SERVICE_HOST=", "PROW_AI_MODEL_PROVIDER_TOKEN="):
    assert forbidden not in environment, environment
PY

runtime_args=(
  --platform linux/amd64
  --read-only
  --user 65532:65532
  --network none
  --cap-drop ALL
  --security-opt no-new-privileges
  --cpus 1
  --memory 1g
  --memory-swap 1g
  --pids-limit 256
  --tmpfs "/tmp:rw,nosuid,nodev,noexec,size=128m,uid=65532,gid=65532,mode=0700"
  --tmpfs "/workspace:rw,nosuid,nodev,exec,size=512m,uid=65532,gid=65532,mode=0700"
)

created_container=$(docker create "${runtime_args[@]}" --entrypoint /bin/sh "$image" -c 'sleep 1')
docker inspect "$created_container" >"$tmp/container-inspect.json"
python3 - "$tmp/container-inspect.json" <<'PY'
import json
import sys

host = json.load(open(sys.argv[1], encoding="utf-8"))[0]["HostConfig"]
assert host["ReadonlyRootfs"] is True, host
assert host["NanoCpus"] == 1_000_000_000, host["NanoCpus"]
assert host["Memory"] == 1_073_741_824, host["Memory"]
assert host["MemorySwap"] == 1_073_741_824, host["MemorySwap"]
assert host["PidsLimit"] == 256, host["PidsLimit"]
assert "ALL" in (host.get("CapDrop") or []), host.get("CapDrop")
security = host.get("SecurityOpt") or []
assert any(value.startswith("no-new-privileges") for value in security), security
tmpfs = host.get("Tmpfs") or {}
assert "/tmp" in tmpfs and "/workspace" in tmpfs, tmpfs
PY
docker rm "$created_container" >/dev/null
created_container=""

security_state=$(docker run --rm "${runtime_args[@]}" --entrypoint /bin/sh "$image" -c '
  set -eu
  test "$(id -u)" = 65532
  test "$(id -g)" = 65532
  test "$(awk "/^CapEff:/ {print \$2}" /proc/self/status)" = 0000000000000000
  test "$(awk "/^NoNewPrivs:/ {print \$2}" /proc/self/status)" = 1
  test "$(awk "/^Seccomp:/ {print \$2}" /proc/self/status)" = 2
  if touch /rootfs-probe 2>/dev/null; then
    echo writable-root >&2
    exit 1
  fi
  touch /workspace/workspace-probe /tmp/tmp-probe
  test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
  for binary in go git opencode fixexecutor; do command -v "$binary" >/dev/null; done
  for binary in gh kubectl; do
    if command -v "$binary" >/dev/null 2>&1; then
      echo "unexpected credential-capable client: $binary" >&2
      exit 1
    fi
  done
  printf "uid=%s gid=%s security=restricted\n" "$(id -u)" "$(id -g)"
')
printf '%s\n' "$security_state"

tool_versions=$(docker run --rm "${runtime_args[@]}" --entrypoint /bin/sh "$image" -c '
  set -eu
  go version
  go env GOTOOLCHAIN
  git version
  opencode --version
')
printf '%s\n' "$tool_versions"
printf '%s\n' "$tool_versions" | grep -Fxq 'go version go1.25.12 linux/amd64'
printf '%s\n' "$tool_versions" | grep -Fxq 'local'
printf '%s\n' "$tool_versions" | grep -Fxq 'git version 2.54.0'
printf '%s\n' "$tool_versions" | grep -Fxq '1.18.2'

# Revalidate the fixed ConfigMap CA mount contract before OpenCode starts.
created_container=$(docker create "$image")
docker cp "$created_container:/etc/ssl/certs/ca-certificates.crt" "$tmp/ca-bundle.pem"
docker rm "$created_container" >/dev/null
created_container=""
ca_hash=$(shasum -a 256 "$tmp/ca-bundle.pem" | awk '{print $1}')
printf 'not a PEM bundle\n' > "$tmp/malformed-ca.pem"
malformed_hash=$(shasum -a 256 "$tmp/malformed-ca.pem" | awk '{print $1}')
run_ca_startup_case() {
  local name=$1 file=${2:-} expected_hash=${3:-} expected_reason=$4
  local args=("${runtime_args[@]}")
  if [[ -n $file ]]; then
    args+=(
      --mount "type=bind,src=${file},dst=/etc/prow-ai-dashboard/model-provider-ca/ca-bundle.pem,readonly"
      --env 'NODE_EXTRA_CA_CERTS=/etc/prow-ai-dashboard/model-provider-ca/ca-bundle.pem'
      --env "PROW_AI_MODEL_PROVIDER_CA_SHA256=${expected_hash}"
    )
  fi
  set +e
  docker run --rm "${args[@]}" "$image" >"$tmp/${name}.json" 2>"$tmp/${name}.err"
  local status=$?
  set -e
  [[ $status -ne 0 ]] || { echo "CA startup case unexpectedly succeeded: $name" >&2; exit 1; }
  python3 - "$tmp/${name}.json" "$expected_reason" <<'PY_CA'
import json
import sys
result = json.load(open(sys.argv[1], encoding="utf-8"))
assert result["version"] == 2, result
assert result["terminal_state"] == "failed", result
assert sys.argv[2] in result["failure_reason"], result
PY_CA
}
run_ca_startup_case disabled '' '' 'PROW_AI_FIX_EXECUTION_REQUEST_B64 is required'
run_ca_startup_case valid "$tmp/ca-bundle.pem" "$ca_hash" 'PROW_AI_FIX_EXECUTION_REQUEST_B64 is required'
run_ca_startup_case wrong-hash "$tmp/ca-bundle.pem" "$(printf '0%.0s' {1..64})" 'SHA-256 does not match configuration'
run_ca_startup_case malformed "$tmp/malformed-ca.pem" "$malformed_hash" 'non-PEM data'

toolchain_guard=$(docker run --rm "${runtime_args[@]}" --entrypoint /bin/sh "$image" -c '
  set -eu
  mkdir /workspace/newer-toolchain
  cd /workspace/newer-toolchain
  printf "module example.com/newer\n\ngo 1.26.0\n" > go.mod
  set +e
  output=$(go list ./... 2>&1)
  status=$?
  set -e
  test "$status" -ne 0
  case "$output" in
    *downloading*) echo "Go attempted a runtime toolchain download" >&2; exit 1 ;;
    *GOTOOLCHAIN=local*) ;;
    *) printf "%s\n" "$output" >&2; exit 1 ;;
  esac
  printf "toolchain-download=disabled\n"
')
printf '%s\n' "$toolchain_guard"

identity=$(docker run --rm "${runtime_args[@]}" "$image" --version)
expected_identity="fixexecutor version=${expected_version} commit=${expected_commit} image_tag=${expected_image_tag}"
[[ "$identity" == "$expected_identity" ]] || {
  echo "fixexecutor identity=$identity" >&2
  exit 1
}
build_info=$(docker run --rm "${runtime_args[@]}" --entrypoint go "$image" version -m /usr/local/bin/fixexecutor)
printf '%s\n' "$build_info" | grep -Fq -- "-X main.commit=${expected_commit}"
printf '%s\n' "$build_info" | grep -Fq -- "-X main.imageTag=${expected_image_tag}"

docker volume create "$source_volume" >/dev/null
source_sha=$(docker run --rm --platform linux/amd64 --network none --user 0:0 \
  --mount "type=volume,src=${source_volume},dst=/repo" \
  --entrypoint /bin/sh "$image" -c '
    set -eu
    cd /repo
    git init -q
    git config commit.gpgsign false
    git config user.name Fixture
    git config user.email fixture@example.test
    cat > go.mod <<"MOD"
module example.com/fixfixture

go 1.25.0
MOD
    cat > message.go <<"GO"
package fixfixture

func Message() string { return "before" }
GO
    cat > message_test.go <<"GO"
package fixfixture

import (
    "bytes"
    "os"
    "path/filepath"
    "testing"
)

func TestMessage(t *testing.T) {
	if got := Message(); got != "after" {
		t.Fatalf("Message() = %q", got)
	}
}

func TestValidationIsolation(t *testing.T) {
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "private-state")); !os.IsNotExist(err) {
		t.Fatalf("OpenCode private state remained visible: %v", err)
	}
	if data, err := os.ReadFile("/proc/1/environ"); err == nil && (
		bytes.Contains(data, []byte("PROW_AI_MODEL_PROVIDER_TOKEN=")) ||
		bytes.Contains(data, []byte("PROW_AI_FIX_EXECUTION_REQUEST_B64="))) {
		t.Fatal("executor parent environment remained visible")
	}
}
GO
    git add go.mod message.go message_test.go
    git commit --no-gpg-sign -qm fixture
    git rev-parse HEAD
    chown -R 65532:65532 /repo
  ')
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "invalid fixture SHA: $source_sha" >&2; exit 1; }

cat >"$tmp/opencode-success" <<'EOF_SUCCESS'
#!/bin/sh
set -eu
printf '%s\n' 'selected private evidence' > "$HOME/private-state"
cat > message.go <<'GO'
package fixfixture

func Message() string { return "after" }
GO
printf '%s\n' '{"type":"text","part":{"text":"credential-free fixture edit complete"}}'
EOF_SUCCESS
cat >"$tmp/opencode-credential" <<'EOF_CREDENTIAL'
#!/bin/sh
set -eu
printf '%s' "$PROW_AI_MODEL_PROVIDER_TOKEN"
EOF_CREDENTIAL
chmod 0755 "$tmp/opencode-success" "$tmp/opencode-credential"

make_request() {
  local mode=$1
  python3 - "$mode" "$source_sha" <<'PY'
import base64
import json
import sys

mode, sha = sys.argv[1:]
provider = {
    "credential_mode": "gateway",
    "api": "chat_completions",
    "endpoint": "https://gateway.example.internal/v1/chat/completions",
    "model": "fixture-model",
    "auth": {"type": "none"},
}
if mode in ("credential", "direct"):
    provider = {
        "credential_mode": "direct",
        "api": "chat_completions",
        "endpoint": "https://api.openai.com/v1/chat/completions",
        "model": "fixture-model",
        "auth": {"type": "bearer", "token_env": "PROW_AI_MODEL_PROVIDER_TOKEN"},
    }
request = {
    "version": 2,
    "repository_url": "file:///input/repository",
    "commit_sha": sha,
    "prompt": "Update Message to return after.",
    "timeout_seconds": 480,
    "max_steps": 8,
    "max_files": 1,
    "command_policy": {
        "allow_shell": False,
        "commands": [
            {"argv": ["go", "version"], "timeout_seconds": 10},
            {"argv": ["go", "test", "./..."], "timeout_seconds": 180},
            {"argv": ["go", "vet", "./..."], "timeout_seconds": 180},
            {"argv": ["git", "diff", "--cached", "--check"], "timeout_seconds": 10},
        ],
    },
    "model_provider": provider,
    "expected_base_sha": sha,
    "output_limit_bytes": 262144,
}
if mode == "warning":
    request["command_policy"]["commands"].insert(
        0, {"argv": ["false"], "timeout_seconds": 10}
    )
print(base64.b64encode(json.dumps(request, separators=(",", ":")).encode()).decode())
PY
}

positive_request=$(make_request success)
set +e
docker run --rm "${runtime_args[@]}" \
  --mount "type=volume,src=${source_volume},dst=/input/repository,readonly" \
  --mount "type=bind,src=${tmp}/opencode-success,dst=/usr/local/bin/opencode,readonly" \
  --env "PROW_AI_FIX_EXECUTION_REQUEST_B64=${positive_request}" \
  "$image" >"$tmp/result.json" 2>"$tmp/result.err"
positive_status=$?
set -e
if [[ $positive_status -ne 0 ]]; then
  cat "$tmp/result.json" "$tmp/result.err" >&2
  exit "$positive_status"
fi
python3 - "$tmp/result.json" "$tmp/change.patch" "$source_sha" <<'PY'
import json
import sys

result_path, patch_path, sha = sys.argv[1:]
result = json.load(open(result_path, encoding="utf-8"))
assert result["version"] == 2, result
assert result["terminal_state"] == "succeeded", result
assert result["base_sha"] == sha, result
assert result["changed_files"] == ["message.go"], result
assert result["files"] == {"message.go": 'package fixfixture\n\nfunc Message() string { return "after" }\n'}, result
assert "credential-free fixture edit complete" in result.get("stdout_summary", ""), result
assert not result.get("failure_reason"), result
commands = result["command_results"]
expected = [
    ["go", "version"],
    ["go", "test", "./..."],
    ["go", "vet", "./..."],
    ["git", "diff", "--cached", "--check"],
]
assert [entry["argv"] for entry in commands] == expected, commands
assert all(entry["exit_code"] == 0 and not entry.get("timed_out", False) for entry in commands), commands
assert "go version go1.25.12 linux/amd64" in commands[0].get("stdout", ""), commands[0]
patch = result["diff"]
assert "diff --git a/message.go b/message.go" in patch, patch
open(patch_path, "w", encoding="utf-8").write(patch)
PY

warning_request=$(make_request warning)
set +e
docker run --rm "${runtime_args[@]}" \
  --mount "type=volume,src=${source_volume},dst=/input/repository,readonly" \
  --mount "type=bind,src=${tmp}/opencode-success,dst=/usr/local/bin/opencode,readonly" \
  --env "PROW_AI_FIX_EXECUTION_REQUEST_B64=${warning_request}" \
  "$image" >"$tmp/warning-result.json" 2>"$tmp/warning-result.err"
warning_status=$?
set -e
if [[ $warning_status -ne 0 ]]; then
  cat "$tmp/warning-result.json" "$tmp/warning-result.err" >&2
  exit "$warning_status"
fi
python3 - "$tmp/warning-result.json" "$source_sha" <<'PY'
import json
import sys

result_path, sha = sys.argv[1:]
result = json.load(open(result_path, encoding="utf-8"))
assert result["version"] == 2, result
assert result["terminal_state"] == "succeeded", result
assert result["base_sha"] == sha, result
assert result["changed_files"] == ["message.go"], result
assert "diff --git a/message.go b/message.go" in result["diff"], result
assert not result.get("failure_reason"), result
commands = result["command_results"]
expected = [
    ["false"],
    ["go", "version"],
    ["go", "test", "./..."],
    ["go", "vet", "./..."],
    ["git", "diff", "--cached", "--check"],
]
assert [entry["argv"] for entry in commands] == expected, commands
assert commands[0]["exit_code"] != 0 and not commands[0].get("timed_out", False), commands[0]
assert all(entry["exit_code"] == 0 and not entry.get("timed_out", False) for entry in commands[1:]), commands
PY

direct_credential='fixture-direct-provider-credential-0123456789abcdef'
direct_request=$(make_request direct)
set +e
docker run --rm "${runtime_args[@]}" \
  --mount "type=volume,src=${source_volume},dst=/input/repository,readonly" \
  --mount "type=bind,src=${tmp}/opencode-success,dst=/usr/local/bin/opencode,readonly" \
  --env "PROW_AI_FIX_EXECUTION_REQUEST_B64=${direct_request}" \
  --env "PROW_AI_MODEL_PROVIDER_TOKEN=${direct_credential}" \
  "$image" >"$tmp/direct-result.json" 2>"$tmp/direct-result.err"
direct_status=$?
set -e
if [[ $direct_status -ne 0 ]]; then
  cat "$tmp/direct-result.json" "$tmp/direct-result.err" >&2
  exit "$direct_status"
fi
if grep -Fq "$direct_credential" "$tmp/direct-result.json" "$tmp/direct-result.err"; then
  echo 'direct provider credential escaped validation isolation' >&2
  exit 1
fi
python3 - "$tmp/direct-result.json" <<'PY'
import json
import sys

result = json.load(open(sys.argv[1], encoding="utf-8"))
assert result["terminal_state"] == "succeeded", result
assert all(entry["exit_code"] == 0 and not entry.get("timed_out", False) for entry in result["command_results"]), result
PY

docker run --rm "${runtime_args[@]}" \
  --mount "type=volume,src=${source_volume},dst=/input/repository,readonly" \
  --mount "type=bind,src=${tmp}/change.patch,dst=/input/change.patch,readonly" \
  --env "SOURCE_SHA=${source_sha}" \
  --entrypoint /bin/sh "$image" -c '
    set -eu
    git clone -q /input/repository /workspace/reconstructed
    cd /workspace/reconstructed
    git checkout -q --detach "$SOURCE_SHA"
    git apply --check /input/change.patch
    git apply --whitespace=nowarn /input/change.patch
    git diff --check
    grep -Fq "return \"after\"" message.go
  '

credential='fixture-provider-credential-0123456789abcdef'
credential_request=$(make_request credential)
set +e
docker run --rm "${runtime_args[@]}" \
  --mount "type=volume,src=${source_volume},dst=/input/repository,readonly" \
  --mount "type=bind,src=${tmp}/opencode-credential,dst=/usr/local/bin/opencode,readonly" \
  --env "PROW_AI_FIX_EXECUTION_REQUEST_B64=${credential_request}" \
  --env "PROW_AI_MODEL_PROVIDER_TOKEN=${credential}" \
  "$image" >"$tmp/credential-result.json" 2>"$tmp/credential-result.err"
credential_status=$?
set -e
[[ $credential_status -ne 0 ]] || { echo 'credential-bearing output succeeded' >&2; exit 1; }
if grep -Fq "$credential" "$tmp/credential-result.json" "$tmp/credential-result.err"; then
  echo 'credential value escaped executor output detection' >&2
  exit 1
fi
python3 - "$tmp/credential-result.json" <<'PY'
import json
import sys

result = json.load(open(sys.argv[1], encoding="utf-8"))
assert result["version"] == 2, result
assert result["terminal_state"] == "failed", result
assert result["failure_reason"] == "credential-bearing executor output rejected", result
assert result.get("files", {}) == {}, result
assert not result.get("changed_files"), result
assert not result.get("diff"), result
PY

printf 'agent-sandbox-fix-executor image contract passed\n'
