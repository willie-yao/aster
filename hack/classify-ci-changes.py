#!/usr/bin/env python3
"""Classify changed paths for dependency-aware CI."""

from __future__ import annotations

import argparse
import os
import pathlib
import subprocess
import sys
import tempfile


CLASSES = (
    "backend",
    "frontend",
    "helm_static",
    "platform_kind",
    "remote_fixer",
    "fix_executor",
    "analysis_images",
    "critic_image",
    "documentation",
    "release_shared",
)


def under(path: str, prefix: str) -> bool:
    return path == prefix or path.startswith(prefix + "/")


def under_any(path: str, prefixes: tuple[str, ...]) -> bool:
    return any(under(path, prefix) for prefix in prefixes)


def documentation_path(path: str) -> bool:
    return (
        under(path, "docs")
        or path.endswith("/README.md")
        or path
        in {
            ".gitignore",
            ".gitattributes",
            "AGENTS.md",
            "CODE_OF_CONDUCT",
            "CODE_OF_CONDUCT.md",
            "CONTRIBUTING.md",
            "LICENSE",
            "README.md",
            "SECURITY.md",
        }
    )


BACKEND_DOCUMENTATION_PATHS = {
    "AGENTS.md",
    "README.md",
    "docs/agent-onboarding.md",
    "docs/onboarding-a-new-project.md",
}

HELM_DOCUMENTATION_PATHS = {
    "AGENTS.md",
    "deploy/helm/aster-platform/README.md",
    "docs/README.md",
    "docs/kubernetes-contributor-deployment.md",
    "docs/kubernetes-platform-administrator.md",
    "docs/kubernetes-platform-ownership.md",
    "docs/kubernetes-platform.md",
    "docs/kubernetes-reference.md",
    "docs/kubernetes.md",
    "docs/onboarding-a-new-project.md",
}


def changed_paths(
    base: str, head: str, *, merge_base: bool, repository: pathlib.Path
) -> list[str]:
    if merge_base:
        base = subprocess.run(
            ["git", "merge-base", base, head],
            cwd=repository,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
        ).stdout.strip()

    output = subprocess.run(
        ["git", "diff", "--no-renames", "--name-only", "-z", base, head],
        cwd=repository,
        check=True,
        stdout=subprocess.PIPE,
    ).stdout
    return [path.decode("utf-8") for path in output.split(b"\0") if path]


REMOTE_BACKEND_PREFIXES = (
    "backend/cmd/aster",
    "backend/cmd/server",
    "backend/cmd/worker",
    "backend/internal/actiondraft",
    "backend/internal/actions",
    "backend/internal/actionverify",
    "backend/internal/agentanalysis",
    "backend/internal/agentsandbox",
    "backend/internal/aggregator",
    "backend/internal/ai",
    "backend/internal/aiusage",
    "backend/internal/analysischat",
    "backend/internal/analysisruntime",
    "backend/internal/artifacts",
    "backend/internal/auth",
    "backend/internal/buildsource",
    "backend/internal/causalcritic",
    "backend/internal/causalfixpreview",
    "backend/internal/chatfix",
    "backend/internal/corrections",
    "backend/internal/fetcher",
    "backend/internal/fetchprogress",
    "backend/internal/fixpr",
    "backend/internal/fixruntime",
    "backend/internal/ghpr",
    "backend/internal/issues",
    "backend/internal/junit",
    "backend/internal/kubernetesdeploy",
    "backend/internal/modelprovider",
    "backend/internal/models",
    "backend/internal/notify",
    "backend/internal/onboard",
    "backend/internal/orka",
    "backend/internal/output",
    "backend/internal/patterns",
    "backend/internal/patternstate",
    "backend/internal/project",
    "backend/internal/prow",
    "backend/internal/prowbuild",
    "backend/internal/redact",
    "backend/internal/remediation",
    "backend/internal/remediationinvestigation",
    "backend/internal/remediationpolicy",
    "backend/internal/repotemplate",
    "backend/internal/resolve",
    "backend/internal/runtime",
    "backend/internal/server",
    "backend/internal/sourceinvestigation",
    "backend/internal/statefile",
    "backend/internal/storage",
    "backend/internal/textutil",
)

ANALYSIS_BACKEND_PREFIXES = (
    "backend/cmd/analysisexecutor",
    "backend/cmd/analysisstager",
    "backend/internal/actionverify",
    "backend/internal/agentanalysis",
    "backend/internal/agentsandbox",
    "backend/internal/ai",
    "backend/internal/aiusage",
    "backend/internal/analysischat",
    "backend/internal/analysisexecutor",
    "backend/internal/analysisruntime",
    "backend/internal/analysisstager",
    "backend/internal/artifacts",
    "backend/internal/buildsource",
    "backend/internal/modelprovider",
    "backend/internal/models",
    "backend/internal/output",
    "backend/internal/project",
    "backend/internal/prowbuild",
    "backend/internal/redact",
    "backend/internal/remediationpolicy",
    "backend/internal/runtime",
    "backend/internal/sourceinvestigation",
    "backend/internal/statefile",
    "backend/internal/storage",
    "backend/internal/textutil",
)

CRITIC_BACKEND_PREFIXES = (
    "backend/cmd/criticexecutor",
    "backend/internal/actionverify",
    "backend/internal/agentanalysis",
    "backend/internal/agentsandbox",
    "backend/internal/ai",
    "backend/internal/aiusage",
    "backend/internal/analysischat",
    "backend/internal/analysisruntime",
    "backend/internal/artifacts",
    "backend/internal/buildsource",
    "backend/internal/causalcritic",
    "backend/internal/criticexecutor",
    "backend/internal/modelprovider",
    "backend/internal/models",
    "backend/internal/output",
    "backend/internal/project",
    "backend/internal/prowbuild",
    "backend/internal/redact",
    "backend/internal/remediationpolicy",
    "backend/internal/runtime",
    "backend/internal/sourceinvestigation",
    "backend/internal/statefile",
    "backend/internal/storage",
    "backend/internal/textutil",
)

FIX_BACKEND_PREFIXES = (
    "backend/cmd/fixexecutor",
    "backend/internal/fixexecutor",
    "backend/internal/modelprovider",
    "backend/internal/runtime",
)

HELM_SUPPORT_PATHS = (
    "hack/test-cli-download-failclosed.sh",
    "hack/test-kubernetes-cleanroom.sh",
    "hack/test-kubernetes-verification-failures.sh",
)

RELEASE_SHARED_PATHS = (
    "Makefile",
    "hack/publish-release.sh",
    "hack/test-publish-release.sh",
    "hack/test-release-cli-assets.sh",
    "hack/test-verify-release-images.sh",
    "hack/verify-release-images.sh",
    "deploy/helm/aster-platform/verify-agent-sandbox-release.sh",
)


def classify(paths: list[str], force_full: bool = False) -> dict[str, bool]:
    result = {name: False for name in CLASSES}

    for raw_path in paths:
        path = raw_path.removeprefix("./")
        if not path:
            continue

        if documentation_path(path):
            result["documentation"] = True
            if path in BACKEND_DOCUMENTATION_PATHS:
                result["backend"] = True
            if path in HELM_DOCUMENTATION_PATHS:
                result["helm_static"] = True
            continue

        matched = False

        if under_any(
            path,
            (
                ".agents/skills/setup-aster-consumer",
                ".agents/skills/author-aster-diagnostics",
            ),
        ):
            result["documentation"] = True
            result["backend"] = True
            matched = True

        if under(path, ".github/workflows") or under(path, ".github/actions"):
            result["release_shared"] = True
            matched = True
        elif under(path, ".github"):
            result["documentation"] = True
            matched = True

        if path in RELEASE_SHARED_PATHS or path == "hack/classify-ci-changes.py":
            result["release_shared"] = True
            matched = True

        if path == "Dockerfile":
            result["backend"] = True
            for name in (
                "remote_fixer",
                "fix_executor",
                "analysis_images",
                "critic_image",
            ):
                result[name] = True
            matched = True

        if under(path, "backend"):
            result["backend"] = True
            matched = True
            if path in {"backend/go.mod", "backend/go.sum"}:
                for name in (
                    "remote_fixer",
                    "fix_executor",
                    "analysis_images",
                    "critic_image",
                ):
                    result[name] = True
            else:
                if under_any(path, REMOTE_BACKEND_PREFIXES):
                    result["remote_fixer"] = True
                if under_any(path, FIX_BACKEND_PREFIXES):
                    result["fix_executor"] = True
                if under_any(path, ANALYSIS_BACKEND_PREFIXES):
                    result["analysis_images"] = True
                if under_any(path, CRITIC_BACKEND_PREFIXES):
                    result["critic_image"] = True

            if (
                under_any(
                    path,
                    (
                        "backend/cmd/fixexecutor",
                        "backend/cmd/analysisexecutor",
                        "backend/cmd/analysisstager",
                        "backend/cmd/criticexecutor",
                        "backend/internal/fixexecutor",
                        "backend/internal/analysisexecutor",
                        "backend/internal/analysisstager",
                        "backend/internal/criticexecutor",
                        "backend/internal/causalcritic",
                        "backend/internal/agentsandbox",
                        "backend/internal/kubernetesdeploy",
                        "backend/internal/modelprovider",
                        "backend/internal/onboard",
                    ),
                )
            ):
                result["helm_static"] = True

        if under(path, "frontend"):
            result["frontend"] = True
            result["remote_fixer"] = True
            matched = True

        if under(path, "configs"):
            result["backend"] = True
            result["helm_static"] = True
            matched = True

        if under(path, "deploy/helm"):
            result["helm_static"] = True
            matched = True
            if under(path, "deploy/helm/aster-platform"):
                result["platform_kind"] = True

        if under(path, "experimental/agent-sandbox"):
            result["helm_static"] = True
            matched = True

        if path in HELM_SUPPORT_PATHS:
            result["helm_static"] = True
            matched = True

        image_test_paths = {
            "hack/test-remote-fixer-image.sh": "remote_fixer",
            "hack/test-agent-sandbox-fix-image.sh": "fix_executor",
            "hack/test-agent-sandbox-analysis-images.sh": "analysis_images",
            "hack/test-agent-sandbox-critic-image.sh": "critic_image",
        }
        if path in image_test_paths:
            result[image_test_paths[path]] = True
            matched = True

        if path in {"deploy/analyzer.Dockerfile", "deploy/fixer.Dockerfile"}:
            result["backend"] = True
            matched = True

        if path in {"hack/install-srt.sh", "hack/build-srt-seccomp.mjs"}:
            result["backend"] = True
            matched = True

        if path == "hack/check-repo-map.sh":
            result["backend"] = True
            matched = True

        if not matched:
            # Unknown paths run the complete suite until they are classified.
            result["release_shared"] = True

    if force_full or result["release_shared"]:
        for name in CLASSES:
            result[name] = True

    return result


def emit(result: dict[str, bool]) -> None:
    for name in CLASSES:
        print(f"{name}={str(result[name]).lower()}")


def self_test() -> None:
    scenarios = (
        ("root documentation contract", ["README.md"], {"backend", "documentation"}),
        (
            "generic documentation",
            ["docs/architecture-decisions/0001-analysis-runtime.md"],
            {"documentation"},
        ),
        (
            "agent onboarding documentation",
            ["docs/agent-onboarding.md"],
            {"backend", "documentation"},
        ),
        (
            "shared onboarding documentation",
            ["docs/onboarding-a-new-project.md"],
            {"backend", "helm_static", "documentation"},
        ),
        (
            "Kubernetes documentation contract",
            ["docs/kubernetes.md"],
            {"helm_static", "documentation"},
        ),
        (
            "platform README contract",
            ["deploy/helm/aster-platform/README.md"],
            {"helm_static", "documentation"},
        ),
        (
            "agent skill contract",
            [".agents/skills/setup-aster-consumer/references/decisions.md"],
            {"backend", "documentation"},
        ),
        (
            "frontend",
            ["frontend/src/App.tsx"],
            {"frontend", "remote_fixer"},
        ),
        (
            "embedded analysis skill",
            ["backend/internal/agentanalysis/skill/failure-analysis.md"],
            {"backend", "remote_fixer", "analysis_images", "critic_image"},
        ),
        (
            "embedded prompt-author skill",
            ["backend/internal/onboard/promptauthor/skill/system-prompt-generation.md"],
            {"backend", "helm_static", "remote_fixer"},
        ),
        (
            "platform",
            ["deploy/helm/aster-platform/values.yaml"],
            {"helm_static", "platform_kind"},
        ),
        (
            "fix executor",
            ["backend/internal/fixexecutor/executor.go"],
            {"backend", "helm_static", "fix_executor"},
        ),
        (
            "shared Dockerfile",
            ["Dockerfile"],
            {
                "backend",
                "remote_fixer",
                "fix_executor",
                "analysis_images",
                "critic_image",
            },
        ),
        (
            "analysis executor",
            ["backend/internal/analysisexecutor/executor.go"],
            {"backend", "helm_static", "analysis_images"},
        ),
        (
            "critic executor",
            ["backend/internal/criticexecutor/executor.go"],
            {"backend", "helm_static", "critic_image"},
        ),
        (
            "remote runtime",
            ["backend/internal/server/server.go"],
            {"backend", "remote_fixer"},
        ),
        ("release", [".github/workflows/release.yml"], set(CLASSES)),
    )

    for name, paths, expected in scenarios:
        actual = {key for key, enabled in classify(paths).items() if enabled}
        if actual != expected:
            raise AssertionError(
                f"{name}: expected {sorted(expected)}, got {sorted(actual)}"
            )

    if {key for key, enabled in classify([], force_full=True).items() if enabled} != set(
        CLASSES
    ):
        raise AssertionError("forced full CI did not enable every class")

    with tempfile.TemporaryDirectory(prefix="aster-ci-classifier-") as tmp:
        repository = pathlib.Path(tmp)

        def git(*args: str) -> str:
            return subprocess.run(
                ["git", *args],
                cwd=repository,
                check=True,
                text=True,
                stdout=subprocess.PIPE,
            ).stdout.strip()

        def write(path: str, content: str) -> None:
            target = repository / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content, encoding="utf-8")

        git("init", "-q")
        git("config", "user.name", "CI Classifier")
        git("config", "user.email", "ci-classifier@example.test")
        write("backend/internal/server/server.go", "package server\n")
        git("add", ".")
        git("commit", "--no-gpg-sign", "-qm", "base")
        common = git("rev-parse", "HEAD")

        git("switch", "-qc", "feature")
        (repository / "docs").mkdir()
        os.rename(
            repository / "backend/internal/server/server.go",
            repository / "docs/archived-server.md",
        )
        git("add", "-A")
        git("commit", "--no-gpg-sign", "-qm", "move backend source into docs")
        feature_head = git("rev-parse", "HEAD")

        git("switch", "-q", "--detach", common)
        write("frontend/src/unrelated.ts", "export {}\n")
        git("add", ".")
        git("commit", "--no-gpg-sign", "-qm", "advance base")
        advanced_base = git("rev-parse", "HEAD")

        paths = changed_paths(
            advanced_base, feature_head, merge_base=True, repository=repository
        )
        expected_paths = {
            "backend/internal/server/server.go",
            "docs/archived-server.md",
        }
        if set(paths) != expected_paths:
            raise AssertionError(
                f"rename/merge-base: expected {sorted(expected_paths)}, got {sorted(paths)}"
            )
        actual = {key for key, enabled in classify(paths).items() if enabled}
        expected = {"backend", "remote_fixer", "documentation"}
        if actual != expected:
            raise AssertionError(
                f"rename/merge-base: expected {sorted(expected)}, got {sorted(actual)}"
            )

    print(f"{len(scenarios) + 2} classification scenarios passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--full", action="store_true", help="enable every CI class")
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--diff-base")
    parser.add_argument("--diff-head")
    parser.add_argument("--merge-base", action="store_true")
    parser.add_argument("paths", nargs="*")
    args = parser.parse_args()

    if args.self_test:
        self_test()
        return 0

    if bool(args.diff_base) != bool(args.diff_head):
        parser.error("--diff-base and --diff-head must be provided together")

    paths = list(args.paths)
    if args.diff_base:
        paths.extend(
            changed_paths(
                args.diff_base,
                args.diff_head,
                merge_base=args.merge_base,
                repository=pathlib.Path.cwd(),
            )
        )

    emit(classify(paths, force_full=args.full))
    return 0


if __name__ == "__main__":
    sys.exit(main())
