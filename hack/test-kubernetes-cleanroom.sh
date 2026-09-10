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
    -run '^(TestScaffold_K8sMode|TestScaffold_K8sStaysFocused|TestK8sDeployReadmeGuidesSafeProjectSpecificInstall|TestK8sDeployReadmeIsProjectAndProviderAgnostic)$' \
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
  "$root" \
  "$tmp" <<'PY'
from pathlib import Path
import os
import runpy
import sys

quickstart = Path(sys.argv[1])
platform = Path(sys.argv[2])
reference = Path(sys.argv[3])
chart = Path(sys.argv[4])
generated = Path(sys.argv[5])
root = Path(sys.argv[6])
fixture_root = Path(sys.argv[7])
checker = runpy.run_path(str(root / "hack" / "check-doc-links.py"))
check_links = checker["check"]
strip_fences = checker["strip_fences"]

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
    Path("docs/kubernetes-contributor-deployment.md"),
    Path("docs/kubernetes-platform-ownership.md"),
    Path("docs/kubernetes-platform-administrator.md"),
]
removed_or_moved_docs = [
    Path("docs/migrating-from-prow-ai-dashboard.md"),
    Path("docs/agent-sandbox-fix-runtime-spike.md"),
    Path("docs/architecture/analysis-runtime-evaluation.md"),
    Path("docs/agent-sandbox-opencode-analyzer.md"),
    Path("docs/maintainer/agent-sandbox-opencode-analyzer.md"),
    Path("docs/remediation-investigation.md"),
    Path("docs/maintainer/remediation-investigation.md"),
]
removed_historical_plans = [
    Path("plan/design-overview-operator-console-1.md"),
    Path("plan/feature-agent-sandbox-critic-experiment-1.md"),
    Path("plan/feature-agent-sandbox-opencode-analyzer-experiment.md"),
]

def require_removed_paths_absent(repository):
    for relative in removed_kubernetes_docs + removed_or_moved_docs + removed_historical_plans:
        if (repository / relative).exists():
            raise SystemExit(f"removed or moved document still exists: {relative}")

require_removed_paths_absent(root)
removed_path_fixture = fixture_root / "removed-path-contract"
active_plan = removed_path_fixture / "plan" / "active-plan.md"
active_plan.parent.mkdir(parents=True)
active_plan.write_text("# Active plan\n")
require_removed_paths_absent(removed_path_fixture)
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
        if str(removed_path) in text or removed_path.name in text:
            raise SystemExit(f"{path} still links removed document {removed_path}")
    for removed_path in removed_or_moved_docs:
        if str(removed_path) in text:
            raise SystemExit(f"{path} still links removed document {removed_path}")

def markdown_tree(base):
    paths = []
    for directory, names, files in os.walk(base, followlinks=False):
        directory = Path(directory)
        names[:] = sorted(name for name in names if not (directory / name).is_symlink())
        for name in sorted(files):
            path = directory / name
            if path.suffix != ".md" or path.is_symlink():
                continue
            try:
                path.resolve().relative_to(repository_root)
            except ValueError:
                continue
            paths.append(path)
    return paths

repository_root = root.resolve()
markdown_files = [
    root / "AGENTS.md",
    root / "README.md",
    root / "CONTRIBUTING.md",
    *markdown_tree(root / "docs"),
    *markdown_tree(root / "experimental"),
]
errors = check_links(repository_root, markdown_files + [chart])
if errors:
    raise SystemExit("\n".join(errors))

retired_feature_terms = [
    "analysis correction",
    "remediation investigation",
]
for path in markdown_tree(root / "docs"):
    visible = strip_fences(path.read_text()).lower()
    for term in retired_feature_terms:
        if term in visible:
            raise SystemExit(f"{path} still documents removed feature {term!r}")

pull_request_guide = root / "docs" / "pull-request-triage.md"
for value in [
    "## Deterministic attribution",
    "## Shared failures",
    "## Optional bot comment",
    "## Optional AI escalation",
    "GITHUB_READ_TOKEN",
    "ASTER_APP_PRIVATE_KEY",
    "server.pullRequestEscalation.enabled",
]:
    if value not in pull_request_guide.read_text():
        raise SystemExit(f"pull request triage guide missing {value!r}")
for path in [root / "docs" / "README.md", root / "docs" / "project-configuration.md"]:
    if "pull-request-triage.md" not in path.read_text():
        raise SystemExit(f"{path} does not link the pull request triage guide")
for heading in [
    "### Shared failures",
    "### Optional bot comment on new pull requests",
    "### Optional AI escalation",
    "### Escalating a shared failure",
]:
    if heading in (root / "docs" / "project-configuration.md").read_text():
        raise SystemExit(f"project configuration duplicates pull request guide section {heading!r}")

PY

python3 "$root/hack/check-doc-links.py" --root "$consumer" deploy/README.md
bash "$root/deploy/helm/aster-platform/test-render.sh"
bash "$root/hack/test-release-cli-assets.sh"
bash "$root/hack/test-kubernetes-verification-failures.sh" \
  "$root/docs/kubernetes.md" "$consumer/deploy/README.md"
bash "$root/hack/test-cli-download-failclosed.sh"
grep -Fq '"--rollback-on-failure"' "$root/backend/internal/kubernetesdeploy/deploy.go"

echo 'Kubernetes clean-room contributor checks passed.'
