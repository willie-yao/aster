#!/usr/bin/env python3
"""Check that supported onboarding selectors use one explicit release."""

from __future__ import annotations

import argparse
import re
import subprocess
import tempfile
from pathlib import Path

RELEASE_ENTRY = re.compile(r"^- \[(v[^]]+)\]")
VERSION = re.compile(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?")
SELECTORS = {
    "go run": re.compile(r"go run github\.com/willie-yao/aster/backend/cmd/aster@([^\s\\]+)"),
    "engine ref": re.compile(r"-engine-ref(?:=|\s+)([^\s\\`\"']+)"),
    "workflow": re.compile(r"willie-yao/aster/\.github/workflows/[^@\s]+@([^\s\\]+)"),
}
EXPECTED_COUNTS = {
    "README.md": {"go run": 1, "engine ref": 1},
    "docs/ai-providers.md": {"workflow": 1},
    "docs/github-pages.md": {"workflow": 3},
    "docs/notifications.md": {"workflow": 1},
    "docs/onboarding-a-new-project.md": {"go run": 3, "engine ref": 2},
    "docs/onboarding-reference.md": {"go run": 1, "engine ref": 1},
    ".agents/skills/setup-aster-consumer/SKILL.md": {"go run": 2, "engine ref": 1},
    ".github/workflows/reusable-clear-cache.yml": {"workflow": 1},
    ".github/workflows/reusable-deploy.yml": {"workflow": 1},
    "backend/internal/onboard/agent_skill_test.go": {"engine ref": 2},
}
CURRENT_PIN_FILES = tuple(EXPECTED_COUNTS)
SUPPORTED_RELEASE_FILE = "docs/supported-onboarding-release.txt"


def changelog_releases(changelog: Path) -> set[str]:
    return {match.group(1) for line in changelog.read_text().splitlines() if (match := RELEASE_ENTRY.match(line))}


def supported_release(root: Path) -> str:
    value = (root / SUPPORTED_RELEASE_FILE).read_text().strip()
    if not VERSION.fullmatch(value):
        raise ValueError(f"{SUPPORTED_RELEASE_FILE}: expected one release tag")
    releases = changelog_releases(root / "CHANGELOG.md")
    if value not in releases:
        raise ValueError(f"{SUPPORTED_RELEASE_FILE}: {value} is not indexed in CHANGELOG.md")
    return value


def verify_paired_tags(root: Path, version: str) -> str:
    revisions = []
    for tag in (version, f"backend/{version}"):
        result = subprocess.run(
            ["git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}"],
            cwd=root, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        if result.returncode != 0:
            raise ValueError(f"missing published onboarding tag {tag}")
        revisions.append(result.stdout.strip())
    if revisions[0] != revisions[1]:
        raise ValueError(f"onboarding tags for {version} do not identify the same commit")
    return revisions[0]


def validate(root: Path, files: tuple[str, ...] = CURRENT_PIN_FILES) -> list[str]:
    errors: list[str] = []
    try:
        expected = supported_release(root)
        verify_paired_tags(root, expected)
    except (OSError, ValueError) as exc:
        return [str(exc)]

    for name in files:
        path = root / name
        try:
            text = path.read_text()
        except OSError as exc:
            errors.append(f"{name}: {exc}")
            continue
        versions = VERSION.findall(text)
        stale = sorted({version for version in versions if version != expected})
        if stale:
            errors.append(f"{name}: found {', '.join(stale)}; expected only {expected}")
        for label, pattern in SELECTORS.items():
            selectors = pattern.findall(text)
            want = EXPECTED_COUNTS.get(name, {}).get(label, 0)
            if len(selectors) != want:
                errors.append(f"{name}: found {len(selectors)} {label} selector(s); expected {want}")
            invalid = sorted({selector for selector in selectors if not selector.startswith("<") and selector != expected})
            if invalid:
                errors.append(f"{name}: {label} selector(s) {', '.join(invalid)} must equal {expected}")
    return errors


def self_test() -> None:
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        (root / "docs").mkdir()
        (root / "CHANGELOG.md").write_text("# Changelog\n\n- [v1.2.3-rc.2](x)\n- [v1.2.3-rc.1](y)\n")
        (root / SUPPORTED_RELEASE_FILE).write_text("v1.2.3-rc.1\n")
        (root / "a.md").write_text("go run github.com/willie-yao/aster/backend/cmd/aster@v1.2.3-rc.1\n")
        subprocess.run(["git", "init", "-q"], cwd=root, check=True)
        subprocess.run(["git", "config", "user.name", "Release Pin Test"], cwd=root, check=True)
        subprocess.run(["git", "config", "user.email", "release-pin@example.test"], cwd=root, check=True)
        subprocess.run(["git", "add", "."], cwd=root, check=True)
        subprocess.run(["git", "commit", "--no-gpg-sign", "-qm", "fixture"], cwd=root, check=True)
        subprocess.run(["git", "-c", "tag.gpgSign=false", "tag", "--no-sign", "v1.2.3-rc.1"], cwd=root, check=True)
        subprocess.run(["git", "-c", "tag.gpgSign=false", "tag", "--no-sign", "backend/v1.2.3-rc.1"], cwd=root, check=True)
        global EXPECTED_COUNTS
        original = EXPECTED_COUNTS
        EXPECTED_COUNTS = {"a.md": {"go run": 1}}
        try:
            if errors := validate(root, ("a.md",)):
                raise AssertionError(errors)
            (root / "a.md").write_text("v1.2.3-rc.1\nuses: willie-yao/aster/.github/workflows/reusable-deploy.yml@main\n")
            errors = validate(root, ("a.md",))
            if not any("@main" in error or "main must equal" in error for error in errors):
                raise AssertionError(f"mutable selector was accepted: {errors}")
            (root / "a.md").write_text("go run github.com/willie-yao/aster/backend/cmd/aster@v1.2.3-rc.2\n")
            errors = validate(root, ("a.md",))
            if not any("v1.2.3-rc.2" in error for error in errors):
                raise AssertionError(f"mixed release was accepted: {errors}")
            (root / SUPPORTED_RELEASE_FILE).write_text("v1.2.3-rc.2\n")
            errors = validate(root, ("a.md",))
            if not any("missing published onboarding tag" in error for error in errors):
                raise AssertionError(f"missing tags were accepted: {errors}")
        finally:
            EXPECTED_COUNTS = original
    print("onboarding release pin self-test passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    root = Path(__file__).resolve().parent.parent
    errors = validate(root)
    if errors:
        for error in errors:
            print(error)
        return 1
    print(f"onboarding release pins match {supported_release(root)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
