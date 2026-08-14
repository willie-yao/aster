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
  "$root/docs/kubernetes.md" \
  "$root/docs/kubernetes-platform.md" \
  "$root/docs/kubernetes-reference.md" \
  "$root/deploy/helm/prow-ai-dashboard-platform/README.md" \
  "$consumer/deploy/README.md" \
  "$root" <<'PY'
from pathlib import Path
import re
import sys

quickstart = Path(sys.argv[1])
platform = Path(sys.argv[2])
reference = Path(sys.argv[3])
chart = Path(sys.argv[4])
generated = Path(sys.argv[5])
root = Path(sys.argv[6])

documents = [quickstart, platform, reference, chart, generated]
for path in documents:
    text = path.read_text()
    for forbidden in [
        "CAPZ",
        "capz",
        "cluster-api-provider-azure",
        "prow-dashboard-demo",
        "<expected-capz-job-name>",
        "Azure",
        "AKS",
        "Front Door",
        "Aster",
        "aster kubernetes",
    ]:
        if forbidden in text:
            raise SystemExit(f"generic Kubernetes document {path} contains {forbidden!r}")

quick = quickstart.read_text()
for value in [
    "prow-ai-dashboard-fetcher-${CLI_VERSION}-${CLI_TARGET}",
    "SHA256SUMS",
    "onboard doctor",
    "kubernetes doctor",
    "--action install",
    "--action upgrade",
    "kubernetes install",
    "kubernetes upgrade",
    "## Verify the first deployment",
    "## Roll back",
    "EXECUTION_NAMESPACE=",
    "EXPECTED_JOB=",
    "PRIOR_CONSUMER_COMMIT",
    "PRIOR_HELM_REVISION",
    "/data/ai_cache.json",
]:
    if value not in quick:
        raise SystemExit(f"Kubernetes quickstart missing {value!r}")

install_doctor = quick.index("--action install")
install = quick.index('"$FETCHER" kubernetes install', install_doctor)
upgrade_doctor = quick.index("--action upgrade")
upgrade = quick.index('"$FETCHER" kubernetes upgrade', upgrade_doctor)
if install_doctor > install or upgrade_doctor > upgrade:
    raise SystemExit("doctor does not precede the contributor write command")

platform_text = platform.read_text()
for value in [
    "Ownership matrix",
    "Agent Sandbox",
    "RuntimeClass",
    "RWX",
    "Cilium",
    "allowedFQDNs",
    "Model gateway and TLS",
    "Secret ownership",
    "Install or upgrade the platform chart",
    "target-cluster acceptance",
    "Upgrade, rollback, and uninstall",
    "helm upgrade --install",
    "rollback",
]:
    if value not in platform_text:
        raise SystemExit(f"Kubernetes platform guide missing {value!r}")

generated_text = generated.read_text()
for value in [
    'export FETCHER="<verified-fetcher-path>"',
    "kubernetes doctor",
    "--action install",
    "--action upgrade",
    "kubernetes install",
    "kubernetes upgrade",
    "rollback",
    "docs/kubernetes.md",
    "docs/kubernetes-platform.md",
    "docs/kubernetes-reference.md",
]:
    if value not in generated_text:
        raise SystemExit(f"generated consumer README missing {value!r}")
for duplicated in ["CLI_ASSET=", "SHA256SUMS", "DOWNLOAD_DIR=", "manifest_ready=", "for _ in"]:
    if duplicated in generated_text:
        raise SystemExit(f"generated consumer README duplicates canonical procedure {duplicated!r}")

removed = [
    "kubernetes-contributor-deployment.md",
    "kubernetes-platform-ownership.md",
    "kubernetes-platform-administrator.md",
]
for name in removed:
    if (root / "docs" / name).exists():
        raise SystemExit(f"removed Kubernetes document still exists: {name}")
for path in [
    root / "AGENTS.md",
    root / "docs" / "README.md",
    root / "docs" / "onboarding-a-new-project.md",
    root / "backend" / "internal" / "onboard" / "templates.go",
    quickstart,
    platform,
    reference,
    chart,
    generated,
]:
    text = path.read_text()
    for name in removed:
        if name in text:
            raise SystemExit(f"{path} still links removed document {name}")

for path in [quickstart, platform, reference, chart]:
    text = path.read_text()
    for target in re.findall(r"\[[^]]+\]\(([^)]+)\)", text):
        target = target.strip().split("#", 1)[0]
        if not target or "://" in target or target.startswith(("mailto:", "#")):
            continue
        resolved = (path.parent / target).resolve()
        if not resolved.exists():
            raise SystemExit(f"broken Markdown link in {path}: {target}")

line_limits = {
    quickstart: (180, 270),
    platform: (150, 220),
    reference: (400, 600),
    chart: (80, 150),
    generated: (120, 200),
}
for path, (minimum, maximum) in line_limits.items():
    lines = len(path.read_text().splitlines())
    if not minimum <= lines <= maximum:
        raise SystemExit(f"{path} has {lines} lines, want {minimum}-{maximum}")

generic_total = sum(len(path.read_text().splitlines()) for path in [quickstart, platform, reference])
if not 900 <= generic_total <= 1200:
    raise SystemExit(f"generic Kubernetes docs total {generic_total} lines, want 900-1200")
PY

bash "$root/deploy/helm/prow-ai-dashboard-platform/test-render.sh"
bash "$root/hack/test-release-cli-assets.sh"
bash "$root/hack/test-kubernetes-verification-failures.sh"
bash "$root/hack/test-cli-download-failclosed.sh"
grep -Fq '"--rollback-on-failure"' "$root/backend/internal/kubernetesdeploy/deploy.go"

echo 'Kubernetes clean-room contributor checks passed.'
