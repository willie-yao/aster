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
  "$root" \
  "$tmp" <<'PY'
from pathlib import Path
import os
import re
import sys

quickstart = Path(sys.argv[1])
platform = Path(sys.argv[2])
reference = Path(sys.argv[3])
chart = Path(sys.argv[4])
generated = Path(sys.argv[5])
root = Path(sys.argv[6])
fixture_root = Path(sys.argv[7])

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
    Path("docs/agent-sandbox-fix-runtime-spike.md"),
    Path("docs/architecture/analysis-runtime-evaluation.md"),
    Path("docs/agent-sandbox-causal-critic.md"),
    Path("docs/agent-sandbox-opencode-analyzer.md"),
    Path("docs/orka.md"),
    Path("docs/remediation-investigation.md"),
]
removed_historical_plans = [
    Path("plan/design-overview-operator-console-1.md"),
    Path("plan/feature-agent-sandbox-critic-experiment-1.md"),
    Path("plan/feature-agent-sandbox-opencode-analyzer-experiment.md"),
    Path("plan/feature-orka-onboarding-prompt-author-1.md"),
    Path("plan/refactor-orka-agent-generation-1.md"),
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
        if removed_path.name in text:
            raise SystemExit(f"{path} still links removed document {removed_path.name}")

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

def markdown_without_fenced_code(text):
    visible = []
    fence = None
    for line in text.splitlines():
        match = re.match(r"^ {0,3}(`{3,}|~{3,})", line)
        if fence is None:
            if match:
                fence = match.group(1)
                visible.append("")
            else:
                visible.append(line)
            continue
        if match and match.group(1)[0] == fence[0] and len(match.group(1)) >= len(fence):
            fence = None
        visible.append("")
    return "\n".join(visible)

def markdown_targets(text):
    text = markdown_without_fenced_code(text)
    inline_pattern = re.compile(
        r"""!?\[[^]\n]+\]\(
            [ \t]*
            (?:
                <([^>\n]+)>
                |
                ((?:\\[^\n]|[^()\s])+)
            )
            (?:
                [ \t]+
                (?:
                    "(?:\\.|[^"\\])*"
                    |
                    '(?:\\.|[^'\\])*'
                    |
                    \((?:\\.|[^)\\])*\)
                )
            )?
            [ \t]*
        \)""",
        re.VERBOSE,
    )
    inline = [match.group(1) or match.group(2) for match in inline_pattern.finditer(text)]
    definitions = []
    definition_pattern = re.compile(
        r"^[ \t]{0,3}\[[^]\n]+\]:[ \t]*(?:<([^>\n]+)>|(\S+))",
        re.MULTILINE,
    )
    for match in definition_pattern.finditer(text):
        definitions.append(match.group(1) or match.group(2))
    return inline, definitions

def external_target(target):
    return target.startswith("//") or re.match(r"^[A-Za-z][A-Za-z0-9+.-]*:", target)

class MarkdownContractError(Exception):
    pass

def validate_markdown_target(path, target, repository_root):
    target = target.strip()
    if target.startswith("<") and target.endswith(">"):
        target = target[1:-1].strip()
    if not target or external_target(target):
        return
    relative, _, anchor = target.partition("#")
    resolved = path.resolve() if not relative else (path.parent / relative).resolve()
    try:
        resolved.relative_to(repository_root)
    except ValueError as err:
        raise MarkdownContractError(
            f"Markdown link escapes repository in {path}: {target}"
        ) from err
    if not resolved.exists():
        raise MarkdownContractError(f"broken Markdown link in {path}: {target}")
    if anchor and resolved.suffix == ".md" and anchor.lower() not in markdown_anchors(resolved):
        raise MarkdownContractError(f"broken Markdown anchor in {path}: {target}")

def validate_markdown(path, repository_root):
    if path.is_symlink():
        raise MarkdownContractError(f"Markdown scan refuses symlink: {path}")
    try:
        path.resolve().relative_to(repository_root)
    except ValueError as err:
        raise MarkdownContractError(f"Markdown source escapes repository: {path}") from err
    inline, definitions = markdown_targets(path.read_text())
    for target in inline + definitions:
        validate_markdown_target(path, target, repository_root)
    return definitions

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
writing_prompt_definitions = []
for path in markdown_files + [chart]:
    definitions = validate_markdown(path, repository_root)
    if path == root / "docs" / "writing-prompts.md":
        writing_prompt_definitions = definitions
for target in [
    "../backend/internal/ai/baseprompt.go",
    "../backend/internal/ai/responseformat.go",
    "../configs/example",
]:
    if target not in writing_prompt_definitions:
        raise SystemExit(f"writing-prompts reference definition was not validated: {target}")

link_fixtures = fixture_root / "markdown-link-contract"
link_fixtures.mkdir()
outside = Path("/etc/hosts")
if not outside.exists():
    raise SystemExit("Markdown traversal fixture requires /etc/hosts")
traversal = link_fixtures / "traversal.md"
traversal.write_text(f"[outside]({os.path.relpath(outside, traversal.parent)})\n")
try:
    validate_markdown(traversal, link_fixtures.resolve())
except MarkdownContractError as err:
    if "escapes repository" not in str(err):
        raise SystemExit(f"traversal fixture failed for the wrong reason: {err}")
else:
    raise SystemExit("out-of-repository Markdown traversal was accepted")

broken_reference = link_fixtures / "broken-reference.md"
broken_reference.write_text("[missing][target]\n\n[target]: missing.md\n")
try:
    validate_markdown(broken_reference, link_fixtures.resolve())
except MarkdownContractError as err:
    if "broken Markdown link" not in str(err):
        raise SystemExit(f"reference fixture failed for the wrong reason: {err}")
else:
    raise SystemExit("broken Markdown reference definition was accepted")

broken_titled = link_fixtures / "broken-titled.md"
broken_titled.write_text('[missing](missing.md "Missing documentation")\n')
try:
    validate_markdown(broken_titled, link_fixtures.resolve())
except MarkdownContractError as err:
    if "missing.md" not in str(err) or "Missing documentation" in str(err):
        raise SystemExit(f"titled link fixture failed for the wrong reason: {err}")
else:
    raise SystemExit("broken titled Markdown destination was accepted")

valid_target = link_fixtures / "target.md"
valid_target.write_text("# Target\n")
valid_reference = link_fixtures / "valid-reference.md"
valid_reference.write_text("""# Fixture

[local][target]
![local image](target.md#target)
[titled link](target.md#target "Documentation index")
![titled image](<target.md#target> "Target image")

[target]: target.md#target
[section]: #fixture
[external]: https://example.com/docs
[email]: mailto:maintainers@example.com

```markdown
[code example](missing.md)
[code-reference]: missing.md
```
""")
validate_markdown(valid_reference, link_fixtures.resolve())

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
