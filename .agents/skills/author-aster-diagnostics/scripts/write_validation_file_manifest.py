#!/usr/bin/env python3
"""Write or compare deterministic validation-engine file manifests."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any


def git(root: Path, *args: str) -> bytes:
    result = subprocess.run(
        ["git", "-C", str(root), *args],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    return result.stdout


def file_bytes(path: Path, mode: int) -> bytes:
    if stat.S_ISLNK(mode):
        return os.fsencode(os.readlink(path))
    return path.read_bytes()


def git_mode(mode: int) -> str:
    if stat.S_ISLNK(mode):
        return "120000"
    if stat.S_ISREG(mode):
        return "100755" if mode & 0o111 else "100644"
    raise ValueError("unsupported file type")


def blob_id(data: bytes) -> str:
    payload = f"blob {len(data)}\0".encode() + data
    return hashlib.sha1(payload, usedforsecurity=False).hexdigest()


def snapshot(root: Path) -> dict[str, Any]:
    root = root.resolve()
    commit = git(root, "rev-parse", "HEAD").decode().strip()
    raw_paths = git(root, "ls-files", "-co", "--exclude-standard", "-z")
    relative_paths = sorted(set(filter(None, raw_paths.split(b"\0"))))
    entries: list[dict[str, str]] = []
    for raw in relative_paths:
        relative = Path(os.fsdecode(raw))
        target = root / relative
        if not target.exists() and not target.is_symlink():
            continue
        mode = target.lstat().st_mode
        if not (stat.S_ISREG(mode) or stat.S_ISLNK(mode)):
            continue
        data = file_bytes(target, mode)
        entries.append({
            "path": relative.as_posix(),
            "mode": git_mode(mode),
            "git_blob_id": blob_id(data),
            "sha256": hashlib.sha256(data).hexdigest(),
        })
    return {
        "schema_version": 1,
        "document_type": "validation_file_manifest",
        "root_commit": commit,
        "entries": entries,
    }


def write_manifest(root: Path, output: Path) -> None:
    value = snapshot(root)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def load_manifest(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"{path}: manifest root must be an object")
    return value


def compare(baseline: Path, final: Path) -> bool:
    return load_manifest(baseline) == load_manifest(final)


def self_test() -> None:
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp) / "repo"
        root.mkdir()
        subprocess.run(["git", "-C", str(root), "init", "-q"], check=True)
        (root / "tracked.txt").write_text("tracked\n")
        executable = root / "tool.sh"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o755)
        subprocess.run(["git", "-C", str(root), "add", "tracked.txt", "tool.sh"], check=True)
        subprocess.run([
            "git", "-C", str(root),
            "-c", "user.name=Manifest Self Test",
            "-c", "user.email=manifest@example.invalid",
            "-c", "commit.gpgsign=false",
            "commit", "-q", "-m", "fixture",
        ], check=True)
        (root / "untracked.txt").write_text("untracked\n")
        first = snapshot(root)
        second = snapshot(root)
        if first != second:
            raise AssertionError("identical file state produced different manifests")
        by_path = {item["path"]: item for item in first["entries"]}
        if set(by_path) != {"tool.sh", "tracked.txt", "untracked.txt"}:
            raise AssertionError(f"unexpected manifest paths: {sorted(by_path)}")
        if by_path["tool.sh"]["mode"] != "100755":
            raise AssertionError("executable mode was not preserved")
        (root / "untracked.txt").write_text("changed\n")
        if first == snapshot(root):
            raise AssertionError("content change was not detected")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--compare", nargs=2, metavar=("BASELINE", "FINAL"), type=Path)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
            print("validation file manifest self-test passed")
            return 0
        if args.compare:
            if args.root or args.output:
                parser.error("--compare cannot be combined with --root or --output")
            if compare(*args.compare):
                print("validation file manifests match")
                return 0
            print("validation file manifests differ", file=sys.stderr)
            return 1
        if not args.root or not args.output:
            parser.error("--root and --output are required when not comparing manifests")
        write_manifest(args.root, args.output)
        print(args.output)
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError, json.JSONDecodeError) as exc:
        print(f"validation file manifest failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
