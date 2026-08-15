#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aster-kubernetes-cleanroom.XXXXXX")
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
  go build -trimpath -o "$tmp/aster" ./cmd/aster
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
"$tmp/aster" onboard doctor --project-dir "$consumer"
"$tmp/aster" kubernetes install \
  --project-dir "$consumer" \
  --values deploy/values.yaml \
  --release sample-dashboard \
  --namespace sample-dashboard \
  --kube-context sample-explicit \
  --chart "$root/deploy/helm/aster" \
  --dry-run
"$tmp/aster" kubernetes upgrade \
  --project-dir "$consumer" \
  --values deploy/values.yaml \
  --release sample-dashboard \
  --namespace sample-dashboard \
  --kube-context sample-explicit \
  --chart "$root/deploy/helm/aster" \
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
  "$root/deploy/helm/aster-platform/README.md" \
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
platform_examples = """Examples only. These are not automatic compatibility guarantees.

| Provider or environment | Example secure runtime |
| --- | --- |
| AKS | Kata or AKS Pod Sandboxing |
| GKE | gVisor or GKE Sandbox |
| EKS | A separately validated sandbox or microVM execution path |
| Self-managed Kubernetes | Kata, gVisor, or equivalent |"""
if platform_examples not in platform.read_text():
    raise SystemExit("Kubernetes platform guide is missing the non-normative provider examples")
for path in documents:
    text = path.read_text()
    if path == platform:
        text = text.replace(platform_examples, "", 1)
    for forbidden in [
        "CAPZ",
        "capz",
        "cluster-api-provider-azure",
        "prow-dashboard-demo",
        "<expected-capz-job-name>",
        "Azure",
        "AKS",
        "GKE",
        "EKS",
        "Front Door",
    ]:
        if forbidden in text:
            raise SystemExit(f"generic Kubernetes document {path} contains {forbidden!r}")

quick = quickstart.read_text()
for value in [
    "aster-${CLI_VERSION}-${CLI_TARGET}",
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
install = quick.index('"$ASTER" kubernetes install', install_doctor)
upgrade_doctor = quick.index("--action upgrade")
upgrade = quick.index('"$ASTER" kubernetes upgrade', upgrade_doctor)
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
    'export ASTER="<verified-aster-path>"',
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

removed_kubernetes_docs = [
    root / "docs" / "kubernetes-contributor-deployment.md",
    root / "docs" / "kubernetes-platform-ownership.md",
    root / "docs" / "kubernetes-platform-administrator.md",
]
removed_or_moved_docs = [
    root / "docs" / "agent-sandbox-fix-runtime-spike.md",
    root / "docs" / "architecture" / "analysis-runtime-evaluation.md",
    root / "docs" / "agent-sandbox-causal-critic.md",
    root / "docs" / "agent-sandbox-opencode-analyzer.md",
    root / "docs" / "orka.md",
    root / "docs" / "remediation-investigation.md",
]
for path in removed_kubernetes_docs + removed_or_moved_docs:
    if path.exists():
        raise SystemExit(f"removed or moved document still exists: {path.relative_to(root)}")
if (root / "plan").exists():
    raise SystemExit("historical plan directory still exists")
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
    for removed_path in removed_kubernetes_docs:
        if removed_path.name in text:
            raise SystemExit(f"{path} still links removed document {removed_path.name}")

for path in [quickstart, platform, reference, chart]:
    text = path.read_text()
    for target in re.findall(r"\[[^]]+\]\(([^)]+)\)", text):
        target = target.strip().split("#", 1)[0]
        if not target or "://" in target or target.startswith(("mailto:", "#")):
            continue
        resolved = (path.parent / target).resolve()
        if not resolved.exists():
            raise SystemExit(f"broken Markdown link in {path}: {target}")

def markdown_anchors(path):
    anchors = set()
    counts = {}
    for line in path.read_text().splitlines():
        match = re.match(r"^#{1,6}\s+(.+?)\s*#*\s*$", line)
        if not match:
            continue
        heading = re.sub(r"`([^`]*)`", r"\1", match.group(1)).lower()
        heading = re.sub(r"<[^>]+>", "", heading)
        heading = re.sub(r"[^\w\- ]", "", heading)
        base = re.sub(r"\s+", "-", heading.strip())
        count = counts.get(base, 0)
        counts[base] = count + 1
        anchors.add(base if count == 0 else f"{base}-{count}")
    return anchors

markdown_files = [
    root / "AGENTS.md",
    root / "README.md",
    root / "CONTRIBUTING.md",
    *sorted((root / "docs").rglob("*.md")),
    *sorted((root / "experimental").rglob("*.md")),
]
for path in markdown_files:
    text = path.read_text()
    for target in re.findall(r"\[[^]]+\]\(([^)]+)\)", text):
        target = target.strip()
        if not target or "://" in target or target.startswith("mailto:"):
            continue
        relative, _, anchor = target.partition("#")
        resolved = path if not relative else (path.parent / relative).resolve()
        if not resolved.exists():
            raise SystemExit(f"broken Markdown link in {path}: {target}")
        if anchor and resolved.suffix == ".md" and anchor.lower() not in markdown_anchors(resolved):
            raise SystemExit(f"broken Markdown anchor in {path}: {target}")

line_limits = {
    quickstart: (180, 270),
    platform: (150, 250),
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

bash "$root/deploy/helm/aster-platform/test-render.sh"
bash "$root/hack/test-release-cli-assets.sh"
bash "$root/hack/test-kubernetes-verification-failures.sh"
bash "$root/hack/test-cli-download-failclosed.sh"
grep -Fq '"--rollback-on-failure"' "$root/backend/internal/kubernetesdeploy/deploy.go"

echo 'Kubernetes clean-room contributor checks passed.'
