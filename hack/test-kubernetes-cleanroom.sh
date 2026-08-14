#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/prow-kubernetes-cleanroom.XXXXXX")
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  find "$tmp" -depth -type d -empty -delete 2>/dev/null || true
}
trap cleanup EXIT

(
  cd "$root/backend"
  go test ./internal/onboard \
    -run '^(TestK8sDeployReadmeGuidesSafeProjectSpecificInstall|TestKubernetesCleanRoomScaffoldContract)$' \
    -count=1
)

consumer=$tmp/consumer
(
  cd "$root/backend"
  CLEANROOM_FIXTURE_OUT="$consumer" go test ./internal/onboard \
    -run '^TestWriteKubernetesCleanRoomFixture$' \
    -count=1
  go build -trimpath -o "$tmp/fetcher" ./cmd/fetcher
)
storage=$tmp/storage
mkdir -p "$storage/logs/sample-e2e-job/1"
printf '{"timestamp":1}\n' > "$storage/logs/sample-e2e-job/1/started.json"
cat > "$consumer/project.yaml" <<PROJECT
id: sample
name: Sample
discovery:
  source: bucket
  exact_jobs:
    - sample-e2e-job
storage:
  provider: local
  base: "$storage"
branding:
  title: Sample
  base_path: /
  site_url: https://dashboard.example.test
  source_repo:
    owner: example
    name: project
PROJECT
python3 - "$consumer/deploy/values.yaml" <<'PY_VALUES'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text().replace('<your-rwx-storage-class>', 'cleanroom-rwx')
path.write_text(text)
PY_VALUES
"$tmp/fetcher" onboard doctor --project-dir "$consumer"
"$tmp/fetcher" kubernetes install \
  --project-dir "$consumer" \
  --values deploy/values.yaml \
  --release sample-dashboard \
  --namespace sample-dashboard \
  --kube-context sample-explicit \
  --chart "$root/deploy/helm/prow-ai-dashboard" \
  --dry-run
"$tmp/fetcher" kubernetes upgrade \
  --project-dir "$consumer" \
  --values deploy/values.yaml \
  --release sample-dashboard \
  --namespace sample-dashboard \
  --kube-context sample-explicit \
  --chart "$root/deploy/helm/prow-ai-dashboard" \
  --dry-run
(
  cd "$root/backend"
  go test ./internal/kubernetesdeploy \
    -run '^(TestRunBuildsUpgradeInstallArguments|TestRunDryRunRendersLocallyWithoutPrintingManifest|TestRunReturnsHelmFailure)$' \
    -count=1
)

python3 - \
  "$root/docs/kubernetes-contributor-deployment.md" \
  "$root/docs/kubernetes-platform-administrator.md" \
  "$root/docs/kubernetes.md" \
  "$root/docs/kubernetes-platform-ownership.md" \
  "$root/docs/kubernetes-reference.md" \
  "$root/deploy/helm/prow-ai-dashboard-platform/README.md" <<'PY'
from pathlib import Path
import sys

contributor = Path(sys.argv[1]).read_text()
admin = Path(sys.argv[2]).read_text()

for raw_path in sys.argv[1:]:
    text = Path(raw_path).read_text()
    for forbidden in [
        "CAPZ",
        "capz",
        "cluster-api-provider-azure",
        "prow-dashboard-demo",
        "<expected-capz-job-name>",
        "Aster",
        "aster kubernetes",
    ]:
        if forbidden in text:
            raise SystemExit(f"generic Kubernetes document {raw_path} contains {forbidden!r}")

required = [
    "prow-ai-dashboard-fetcher-${CLI_VERSION}-${CLI_TARGET}",
    "SHA256SUMS",
    "kubernetes doctor",
    "-action install",
    "-action upgrade",
    "kubernetes install",
    "kubernetes upgrade",
    "rollback",
    "matching chart version",
    "SERVER_DEPLOYMENT=",
    "WORKER_DEPLOYMENT=",
    "SERVER_SERVICE=",
    "EXECUTION_NAMESPACE=",
    "EXPECTED_JOB=",
    "PRIOR_CONSUMER_COMMIT",
    "PRIOR_HELM_REVISION",
    "set -euo pipefail",
    "manifest_ready=false",
    "trap cleanup EXIT",
    "CLI download or verification failed",
    'test "$PRIVATE_STATUS" = 404',
]
for value in required:
    if value not in contributor:
        raise SystemExit(f"contributor guide missing {value!r}")

install_doctor = contributor.index("-action install")
install = contributor.index('"$FETCHER" kubernetes install', install_doctor)
upgrade_doctor = contributor.index("-action upgrade")
upgrade = contributor.index('"$FETCHER" kubernetes upgrade', upgrade_doctor)
if install_doctor > install or upgrade_doctor > upgrade:
    raise SystemExit("doctor does not precede the write command")

unsupported_commands = [
    "git clone https://github.com/willie-yao/prow-ai-dashboard",
    "make -C",
    "kubectl config set-cluster",
    "az afd",
    "--from-literal",
]
for value in unsupported_commands:
    if value in contributor:
        raise SystemExit(f"contributor guide contains unsupported command {value!r}")

for value in [
    "v0.5.3",
    "prow-ai-dashboard-platform",
    "RuntimeClass",
    "Secret manager",
    "external edge",
    "rollback",
    "Uninstall retains the execution namespace",
    "export ENGINE_TAG=",
    "export APPLICATION_RELEASE=",
    "export EXECUTION_NAMESPACE=",
    "export RWX_STORAGE_CLASS=",
    "export AI_SECRET_NAME=",
    "export PUBLIC_URL=",
    "EXPECTED_JOB=<expected-job-name>",
]:
    if value not in admin:
        raise SystemExit(f"platform administrator guide missing {value!r}")
PY

bash "$root/deploy/helm/prow-ai-dashboard-platform/test-render.sh"
bash "$root/hack/test-release-cli-assets.sh"
bash "$root/hack/test-kubernetes-verification-failures.sh"
bash "$root/hack/test-cli-download-failclosed.sh"
grep -Fq '"--rollback-on-failure"' "$root/backend/internal/kubernetesdeploy/deploy.go"

echo 'Kubernetes clean-room contributor checks passed.'
