#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
upgrade=$root/deploy/helm/upgrade.sh
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-upgrade-$$"
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
case \$command in
  status)
    if [[ " \$* " == *' -o json '* ]]; then
      printf '{"version":17}\n'
    fi
    ;;
  get)
    resource=\${1:-}
    if [[ \$resource == values ]]; then
      printf '{"analysisCache":{"generation":"%s"},"image":{"tag":""}}\n' "\${FAKE_CACHE_GENERATION:-cache-7}"
    elif [[ \$resource == manifest ]]; then
      if [[ -f "$state" ]]; then
        tag=\$(cat "$state")
      else
        tag=sha-1111111
      fi
      cat <<MANIFEST
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ghcr.io/willie-yao/prow-ai-dashboard:\$tag
        - name: fixer
          image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:\$tag
      args:
        - -orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:\$tag
      initContainers:
        - name: materializer
          image: busybox:1.36.1
MANIFEST
    fi
    ;;
  lint)
    ;;
  template)
    tag=missing
    for arg in "\$@"; do
      case \$arg in
        global.imageTag=*) tag=\${arg#global.imageTag=} ;;
      esac
    done
    fixer_tag=\$tag
    if [[ \${FAKE_MUTABLE_IMAGE:-false} == true ]]; then
      fixer_tag=main
    fi
    cat <<MANIFEST
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ghcr.io/willie-yao/prow-ai-dashboard:\$tag
        - name: fixer
          image: ghcr.io/willie-yao/prow-ai-dashboard/fixer:\$fixer_tag
      args:
        - -orka-analysis-image=ghcr.io/willie-yao/prow-ai-dashboard/analyzer:\$tag
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
if [[ \${FAKE_IMAGE_FAILURE:-false} == true && \$3 == *'/analyzer:'* ]]; then
  exit 1
fi
EOF_DOCKER
chmod +x "$tmp/bin/docker"

cat > "$tmp/consumer-values.yaml" <<'VALUES'
global:
  imageTag: ""
image:
  tag: ""
analysisRuntime:
  orkaContainer:
    image:
      tag: ""
orka:
  fixRuntime:
    image:
      tag: ""
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
grep -Fq "lint <$root/deploy/helm/prow-ai-dashboard>" "$calls"
grep -Fq "template <capz> <$root/deploy/helm/prow-ai-dashboard>" "$calls"
grep -Fq "<-f> <$tmp/consumer-values.yaml>" "$calls"
grep -Fq '<--set-string> <global.imageTag=sha-deadbeef>' "$calls"
grep -Fq '<--set-string> <analysisCache.generation=cache-7>' "$calls"
grep -Fq "upgrade <capz> <$root/deploy/helm/prow-ai-dashboard>" "$calls"
grep -Fq '<--reuse-values>' "$calls"
grep -Fq "<--values> <$tmp/consumer-values.yaml>" "$calls"
grep -Fq '<--wait> <--rollback-on-failure>' "$calls"
grep -Fxq 'busybox:1.36.1' "$inspections"
grep -Fxq 'ghcr.io/willie-yao/prow-ai-dashboard:sha-deadbeef' "$inspections"
grep -Fxq 'ghcr.io/willie-yao/prow-ai-dashboard/analyzer:sha-deadbeef' "$inspections"
grep -Fxq 'ghcr.io/willie-yao/prow-ai-dashboard/fixer:sha-deadbeef' "$inspections"
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
