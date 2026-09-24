#!/usr/bin/env python3
"""Validate newly indexed release notes in a pull request, without writing tags."""

import argparse
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


INDEX = re.compile(r"^- \[([^]\n]+)\]\(changelog/([^)\n]+)\)", re.MULTILINE)
SHA = re.compile(r"^[0-9a-f]{40}$")


def git(*args, cwd=None):
    return subprocess.check_output(("git", *args), cwd=cwd)


def index_entries(contents):
    return {(label, path) for label, path in INDEX.findall(contents) if label.startswith("v")}


def release_intent(base_sha, cwd=None):
    if not SHA.fullmatch(base_sha):
        raise ValueError("invalid release PR base commit")
    git("cat-file", "-e", f"{base_sha}^{{commit}}", cwd=cwd)
    if subprocess.run(
        ("git", "merge-base", "--is-ancestor", base_sha, "HEAD"), cwd=cwd, check=False
    ).returncode != 0:
        raise ValueError("release PR base is not an ancestor of HEAD")

    fields = git("diff", "--name-status", "-z", "--no-renames", base_sha, "HEAD", cwd=cwd).decode().split("\0")
    changes = list(zip(fields[::2], fields[1::2]))
    added_notes = {
        path
        for status, path in changes
        if status == "A" and path.startswith("changelog/v") and path.endswith(".md")
    }
    before = git("show", f"{base_sha}:CHANGELOG.md", cwd=cwd).decode()
    after = (Path(cwd or ".") / "CHANGELOG.md").read_text()
    added_entries = index_entries(after) - index_entries(before)

    if not added_notes and not added_entries:
        return []
    if index_entries(before) - index_entries(after):
        raise ValueError("a release-intent PR may not remove existing release entries")
    expected = {(Path(path).stem, Path(path).name) for path in added_notes}
    if added_entries != expected:
        raise ValueError("new release notes and CHANGELOG.md entries must match exactly")
    if {path for _, path in changes} != added_notes | {"CHANGELOG.md"}:
        raise ValueError("a release-intent PR may only add its notes and update CHANGELOG.md")
    return sorted(tag for tag, _ in expected)


def self_test():
    with tempfile.TemporaryDirectory(prefix="aster-release-intent-") as tmp:
        repo = Path(tmp)
        git("init", "-q", "-b", "main", cwd=repo)
        git("config", "user.name", "Release Intent Test", cwd=repo)
        git("config", "user.email", "release-test@example.test", cwd=repo)
        git("config", "commit.gpgSign", "false", cwd=repo)
        (repo / "changelog").mkdir()
        (repo / "CHANGELOG.md").write_text("# Changelog\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "base", cwd=repo)
        base = git("rev-parse", "HEAD", cwd=repo).decode().strip()

        assert release_intent(base, repo) == []
        (repo / "changelog/v1.2.3.md").write_text("Notes.\n")
        (repo / "CHANGELOG.md").write_text("# Changelog\n- [v1.2.3](changelog/v1.2.3.md)\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "release", cwd=repo)
        assert release_intent(base, repo) == ["v1.2.3"]
        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()

        (repo / "changelog/v1.2.3.md").write_text("Corrected notes.\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "correct notes", cwd=repo)
        assert release_intent(old, repo) == []
        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()

        (repo / "CHANGELOG.md").write_text(
            "# Changelog\n- [v1.2.3](changelog/v1.2.3.md)\n- [v1.2.4](changelog/v1.2.4.md)\n"
        )
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "orphan index", cwd=repo)
        try:
            release_intent(old, repo)
        except ValueError:
            pass
        else:
            raise AssertionError("accepted an index entry without new release notes")

        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()
        (repo / "changelog/v1.2.4.md").write_text("Late notes.\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "unindexed notes", cwd=repo)
        try:
            release_intent(old, repo)
        except ValueError:
            pass
        else:
            raise AssertionError("accepted newly added notes without a new index entry")

        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()
        for tag in ("v1.2.5", "v1.2.6"):
            (repo / f"changelog/{tag}.md").write_text("Notes.\n")
            with (repo / "CHANGELOG.md").open("a") as index:
                index.write(f"- [{tag}](changelog/{tag}.md)\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "two releases", cwd=repo)
        assert release_intent(old, repo) == ["v1.2.5", "v1.2.6"]

        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()
        (repo / "README.md").write_text("Unrelated documentation.\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "ordinary docs", cwd=repo)
        assert release_intent(old, repo) == []

        old = git("rev-parse", "HEAD", cwd=repo).decode().strip()
        (repo / "changelog/v1.2.7.md").write_text("Notes.\n")
        with (repo / "CHANGELOG.md").open("a") as index:
            index.write("- [v1.2.7](changelog/v1.2.7.md)\n")
        (repo / "README.md").write_text("Mixed with a release.\n")
        git("add", ".", cwd=repo)
        git("commit", "-q", "-m", "mixed intent", cwd=repo)
        try:
            release_intent(old, repo)
        except ValueError:
            pass
        else:
            raise AssertionError("accepted unrelated files in a release-intent PR")
    print("Release PR intent checks passed.")


def main():
    if sys.argv[1:] == ["--self-test"]:
        self_test()
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-sha", required=True)
    parser.add_argument("--base-ref", required=True)
    args = parser.parse_args()
    for tag in release_intent(args.base_sha):
        env = os.environ.copy()
        env.update(TAG=tag, RELEASE_SOURCE_BRANCH=args.base_ref, PREPARE_REVIEW_ONLY="true")
        subprocess.run(("bash", "hack/prepare-release-tag.sh"), env=env, check=True)
    print("Release PR preflight passed.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as exc:
        raise SystemExit(str(exc)) from exc
