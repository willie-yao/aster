#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
upgrade=$root/deploy/helm/upgrade.sh
tmp="$root/.test-work/aster-upgrade-$$"
if [[ -e $tmp ]]; then
  echo "scratch path already exists: $tmp" >&2
  exit 1
fi
mkdir -p "$tmp/bin"
trap 'rm -rf "$tmp"' EXIT

calls=$tmp/helm-calls
inspections=$tmp/image-inspections
state=$tmp/upgraded

cat > "$tmp/bin/helm" <<EOF_HELM
#!/usr/bin/env bash
set -euo pipefail
printf '%s' "\$1" >> "$calls"
for arg in "\${@:2}"; do
  printf ' <%s>' "\$arg" >> "$calls"
done
printf '\n' >> "$calls"

command=\${1:-}
shift || true
if [[ \$command == lint || \$command == template || \$command == upgrade ]]; then
  for arg in "\$@"; do
    if [[ \${arg##*/} == candidate-values.json ]]; then
      cp "\$arg" "$tmp/candidate-\$command.json"
    fi
  done
fi
case \$command in
  status)
    if [[ " \$* " == *' -o json '* ]]; then
      printf '{"version":17}\n'
    fi
    ;;
  get)
    resource=\${1:-}
    if [[ \$resource == values ]]; then
      printf '{"analysisCache":{"generation":"%s"},"analysisRuntime":{"type":"inprocess"},"image":{"tag":""},"agentSandbox":{"fixRuntime":{"enabled":true,"maxSteps":30,"maxFiles":3,"timeout":"10m","outputLimitBytes":524288,"allowedCommands":[{"argv":["git","diff","--cached","--check"],"timeout":"1m"}]}},"server":{"actions":{"enabled":true,"mode":"oauth","oauth":{"clientId":"client","clientSecret":"secret","redirectUrl":"https://dashboard.test/api/auth/callback","sessionKey":"session-key","botToken":"bot-token","privateRepositories":true,"scope":"repo","chatScope":"read:user"}},"extraEnv":[{"name":"OAUTH_SCOPE","value":"repo"},{"name":"KEEP_ME","value":"kept"}]}}\n' "\${FAKE_CACHE_GENERATION:-cache-7}"
    elif [[ \$resource == manifest ]]; then
      if [[ -f "$state" ]]; then
        tag=\$(cat "$state")
      else
        tag=sha-1111111
      fi
      remote_fixer_tag=\$tag
      cat <<MANIFEST
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ghcr.io/willie-yao/aster:\$tag
        - name: remote-fixer
          image: ghcr.io/willie-yao/aster/remote-fixer:\$remote_fixer_tag
      initContainers:
        - name: materializer
          image: busybox:1.36.1
MANIFEST
    fi
    ;;
  lint)
    ;;
  template)
    if [[ \${2:-} == */value-merge ]]; then
      merge_files=()
      while ((\$#)); do
        case \$1 in
          --values|-f)
            merge_files+=("\$2")
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      python3 - "\${merge_files[@]}" <<'PY_MERGE'
import json
import sys

merged = {}
for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as values_file:
        incoming = json.load(values_file)
    def merge(left, right):
        for key, value in right.items():
            if isinstance(value, dict) and isinstance(left.get(key), dict):
                merge(left[key], value)
            else:
                left[key] = value
    merge(merged, incoming)
print("---")
print("# Source: value-merge/templates/values.json")
print(json.dumps(merged, separators=(",", ":")))
PY_MERGE
      exit 0
    fi
    tag=missing
    for arg in "\$@"; do
      case \$arg in
        global.imageTag=*) tag=\${arg#global.imageTag=} ;;
      esac
    done
    remote_fixer_tag=\$tag
    if [[ \${FAKE_MUTABLE_IMAGE:-false} == true ]]; then
      remote_fixer_tag=main
    fi
    cat <<MANIFEST
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ghcr.io/willie-yao/aster:\$tag
        - name: remote-fixer
          image: ghcr.io/willie-yao/aster/remote-fixer:\$remote_fixer_tag
      initContainers:
        - name: materializer
          image: busybox:1.36.1
MANIFEST
    ;;
  upgrade)
    if [[ \${1:-} == --help ]]; then
      if [[ \${FAKE_UNSUPPORTED_HELM:-false} == true ]]; then
        printf '%s\n' 'Usage: helm upgrade'
      else
        printf '%s\n' '      --rollback-on-failure'
      fi
      exit 0
    fi
    tag=missing
    for arg in "\$@"; do
      case \$arg in
        global.imageTag=*) tag=\${arg#global.imageTag=} ;;
      esac
    done
    printf '%s\n' "\$tag" > "$state"
    ;;
  *)
    echo "unexpected helm command: \$command" >&2
    exit 1
    ;;
esac
EOF_HELM
chmod +x "$tmp/bin/helm"

cat > "$tmp/bin/docker" <<EOF_DOCKER
#!/usr/bin/env bash
set -euo pipefail
if [[ \${1:-} != manifest || \${2:-} != inspect || -z \${3:-} ]]; then
  echo "unexpected docker invocation: \$*" >&2
  exit 1
fi
printf '%s\n' "\$3" >> "$inspections"
if [[ \${FAKE_IMAGE_FAILURE:-false} == true && \$3 == *'/remote-fixer:'* ]]; then
  exit 1
fi
EOF_DOCKER
chmod +x "$tmp/bin/docker"

cat > "$tmp/consumer-values.yaml" <<'VALUES'
{
  "global": {"imageTag": ""},
  "image": {"tag": ""}
}
VALUES

export PATH="$tmp/bin:/usr/bin:/bin"

"$upgrade" \
  --context h100 \
  --namespace capz-dynamo \
  --release capz \
  --version sha-deadbeef \
  --values "$tmp/consumer-values.yaml" > "$tmp/upgrade-output"

grep -Fq 'status <capz> <--kube-context> <h100> <--namespace> <capz-dynamo>' "$calls"
grep -Fq 'get <values> <capz> <--kube-context> <h100> <--namespace> <capz-dynamo> <-o> <json>' "$calls"
grep -Fq "lint <$root/deploy/helm/aster>" "$calls"
grep -Fq "template <capz> <$root/deploy/helm/aster>" "$calls"
grep -Fq "<--values> <$tmp/consumer-values.yaml>" "$calls"
grep -Fq '<--set-string> <global.imageTag=sha-deadbeef>' "$calls"
grep -Fq '<--set-string> <analysisCache.generation=cache-7>' "$calls"
grep -Fq "upgrade <capz> <$root/deploy/helm/aster>" "$calls"
if grep -Fq '<--reuse-values>' "$calls"; then
  echo 'guarded upgrade retained --reuse-values' >&2
  exit 1
fi
grep -Fq '<--reset-values>' "$calls"
grep -Fq "<--values> <$tmp/consumer-values.yaml>" "$calls"
grep -Fq '<--wait> <--rollback-on-failure>' "$calls"
for command in lint template upgrade; do
  test -s "$tmp/candidate-$command.json"
done
cmp "$tmp/candidate-lint.json" "$tmp/candidate-template.json"
cmp "$tmp/candidate-lint.json" "$tmp/candidate-upgrade.json"
python3 - "$tmp/candidate-upgrade.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as candidate_file:
    values = json.load(candidate_file)
if "analysisRuntime" in values:
    raise SystemExit("removed analysis runtime selector survived")
oauth = values["server"]["actions"]["oauth"]
for key in ("scope", "chatScope", "privateRepositories"):
    if key in oauth:
        raise SystemExit(f"deprecated OAuth key survived: {key}")
if oauth.get("botToken") != "bot-token":
    raise SystemExit("BOT_TOKEN value was not preserved")
env = values["server"].get("extraEnv", [])
if env != [{"name": "KEEP_ME", "value": "kept"}]:
    raise SystemExit(f"unexpected sanitized extraEnv: {env!r}")
fix_runtime = values.get("agentSandbox", {}).get("fixRuntime", {})
for key in ("maxSteps", "maxFiles", "timeout", "outputLimitBytes", "allowedCommands"):
    if key in fix_runtime:
        raise SystemExit(f"stale fix runtime bound survived: {key}")
if fix_runtime.get("enabled") is not True:
    raise SystemExit("fix runtime enablement was not preserved")
PY
grep -Fq 'Removed deprecated controls from the candidate values:' "$tmp/upgrade-output"
grep -Fq 'analysisRuntime' "$tmp/upgrade-output"
grep -Fq 'server.actions.oauth.privateRepositories' "$tmp/upgrade-output"
grep -Fq 'server.actions.oauth.scope' "$tmp/upgrade-output"
grep -Fq 'server.actions.oauth.chatScope' "$tmp/upgrade-output"
grep -Fq 'server.extraEnv[OAUTH_SCOPE]' "$tmp/upgrade-output"
grep -Fq 'agentSandbox.fixRuntime.maxSteps' "$tmp/upgrade-output"
grep -Fq 'agentSandbox.fixRuntime.allowedCommands' "$tmp/upgrade-output"
grep -Fxq 'busybox:1.36.1' "$inspections"
grep -Fxq 'ghcr.io/willie-yao/aster:sha-deadbeef' "$inspections"
grep -Fxq 'ghcr.io/willie-yao/aster/remote-fixer:sha-deadbeef' "$inspections"
grep -Fq 'Preserving analysis cache generation: cache-7' "$tmp/upgrade-output"
grep -Fq 'Image changes:' "$tmp/upgrade-output"
grep -Fq 'Helm revision: 17' "$tmp/upgrade-output"
grep -Fq 'Deployed image references:' "$tmp/upgrade-output"
if grep -Eq 'create job|delete job|clear.?cache' "$calls"; then
  echo 'upgrade helper attempted a cache clear or manual Job operation' >&2
  exit 1
fi

calls_before=$(wc -l < "$calls")
for invalid_version in latest main sha-short v1; do
  if "$upgrade" --context h100 --namespace capz-dynamo --release capz \
    --version "$invalid_version" > "$tmp/invalid-$invalid_version" 2>&1; then
    echo "upgrade helper accepted invalid version $invalid_version" >&2
    exit 1
  fi
done
if [[ $(wc -l < "$calls") -ne $calls_before ]]; then
  echo 'invalid version reached Helm' >&2
  exit 1
fi

if "$upgrade" --context h100 --namespace capz-dynamo --release capz \
  > "$tmp/missing-version" 2>&1; then
  echo 'upgrade helper accepted missing required arguments' >&2
  exit 1
fi
grep -Fq -- '--version is required' "$tmp/missing-version"

helm_calls_before=$(wc -l < "$calls")
if FAKE_UNSUPPORTED_HELM=true "$upgrade" \
  --context h100 --namespace capz-dynamo --release capz \
  --version sha-cafebabe > "$tmp/unsupported-helm" 2>&1; then
  echo 'upgrade helper accepted Helm without rollback-on-failure support' >&2
  exit 1
fi
grep -Fq 'Helm 4 with --rollback-on-failure support is required' "$tmp/unsupported-helm"
if [[ $(wc -l < "$calls") -ne $((helm_calls_before + 1)) ]]; then
  echo 'unsupported Helm reached release inspection' >&2
  exit 1
fi

upgrades_before=$(grep -c '^upgrade <capz>' "$calls")
if FAKE_MUTABLE_IMAGE=true "$upgrade" \
  --context h100 --namespace capz-dynamo --release capz \
  --version sha-cafebabe --values "$tmp/consumer-values.yaml" \
  > "$tmp/mutable-candidate" 2>&1; then
  echo 'upgrade helper accepted a mutable image-specific tag' >&2
  exit 1
fi
grep -Fq 'candidate image must use an immutable' "$tmp/mutable-candidate"
if [[ $(grep -c '^upgrade <capz>' "$calls") -ne $upgrades_before ]]; then
  echo 'mutable candidate reached Helm upgrade' >&2
  exit 1
fi

if FAKE_IMAGE_FAILURE=true "$upgrade" \
  --context h100 --namespace capz-dynamo --release capz \
  --version sha-cafebabe --values "$tmp/consumer-values.yaml" \
  > "$tmp/missing-image" 2>&1; then
  echo 'upgrade helper accepted a missing image manifest' >&2
  exit 1
fi
grep -Fq 'image manifest is unavailable' "$tmp/missing-image"
if [[ $(grep -c '^upgrade <capz>' "$calls") -ne $upgrades_before ]]; then
  echo 'missing image reached Helm upgrade' >&2
  exit 1
fi

FAKE_CACHE_GENERATION=0 "$upgrade" \
  --context h100 \
  --namespace capz-dynamo \
  --release capz \
  --version v1.2.3-rc.1 \
  --values "$tmp/consumer-values.yaml" \
  --skip-image-check > "$tmp/semver-output"
grep -Fq '<--set-string> <global.imageTag=v1.2.3-rc.1>' "$calls"
grep -Fq '<--set-string> <analysisCache.generation=0>' "$calls"
grep -Fq 'Preserving analysis cache generation: 0' "$tmp/semver-output"
grep -Fq 'Skipping image manifest checks by explicit request.' "$tmp/semver-output"

echo 'Helm upgrade helper checks passed.'
