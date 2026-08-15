#!/usr/bin/env python3
"""Mediate and log pre-freeze filesystem reads for blind evaluations."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any


def load_object(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"{path}: expected a JSON object")
    return value


def next_id(log: Path) -> str:
    if not log.exists():
        return "access-0001"
    count = sum(1 for line in log.read_text().splitlines() if line.strip())
    return f"access-{count + 1:04d}"


def append(log: Path, record: dict[str, Any]) -> None:
    log.parent.mkdir(parents=True, exist_ok=True)
    with log.open("a") as stream:
        stream.write(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n")


def decide(policy: dict[str, Any], phase: str, category: str) -> str:
    denied = set(policy.get("deny_categories", []))
    if phase == "pre_freeze" and category in denied:
        return "blocked"
    return "allowed"


def directory_listing(path: Path) -> bytes:
    rows = []
    for child in sorted(path.iterdir(), key=lambda item: item.name):
        kind = "symlink" if child.is_symlink() else "directory" if child.is_dir() else "file"
        rows.append({"name": child.name, "kind": kind})
    return (json.dumps(rows, indent=2, sort_keys=True) + "\n").encode()


def access(args: argparse.Namespace) -> int:
    policy = load_object(args.policy)
    decision = decide(policy, args.phase, args.category)
    record = {
        "id": next_id(args.log),
        "phase": args.phase,
        "category": args.category,
        "path": str(args.path),
        "purpose": args.purpose,
        "decision": decision,
        "access_method": "wrapper",
        "content_sha256": None,
        "bytes_read": None,
    }
    if decision == "blocked":
        append(args.log, record)
        print(f"blocked pre-freeze access to category {args.category}", file=sys.stderr)
        return 3

    if args.command == "read":
        data = args.path.read_bytes()
    else:
        if not args.path.is_dir():
            raise ValueError(f"{args.path}: list requires a directory")
        data = directory_listing(args.path)
    record["content_sha256"] = hashlib.sha256(data).hexdigest()
    record["bytes_read"] = len(data)
    append(args.log, record)
    sys.stdout.buffer.write(data)
    return 0


def self_test() -> None:
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp)
        policy = root / "policy.json"
        log = root / "access.jsonl"
        allowed = root / "allowed.txt"
        denied = root / "denied.txt"
        allowed.write_text("allowed\n")
        denied.write_text("denied\n")
        policy.write_text(json.dumps({"deny_categories": ["prior_diagnosis"]}) + "\n")
        common = argparse.Namespace(policy=policy, log=log, phase="pre_freeze", purpose="self-test")
        common.command, common.path, common.category = "read", allowed, "source_code"
        if access(common) != 0:
            raise AssertionError("allowed read failed")
        common.command, common.path, common.category = "read", denied, "prior_diagnosis"
        if access(common) != 3:
            raise AssertionError("denylisted read was not blocked")
        records = [json.loads(line) for line in log.read_text().splitlines()]
        if records[0]["decision"] != "allowed" or records[0]["content_sha256"] is None:
            raise AssertionError("allowed access was not hashed")
        if records[1]["decision"] != "blocked" or records[1]["bytes_read"] is not None:
            raise AssertionError("blocked access read content")
        if denied.read_text() != "denied\n":
            raise AssertionError("self-test changed evidence")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["read", "list"], nargs="?")
    parser.add_argument("--policy", type=Path)
    parser.add_argument("--log", type=Path)
    parser.add_argument("--path", type=Path)
    parser.add_argument("--category")
    parser.add_argument("--phase", choices=["pre_freeze", "post_reveal"], default="pre_freeze")
    parser.add_argument("--purpose")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
            print("blind access self-test passed")
            return 0
        for field in ("command", "policy", "log", "path", "category", "purpose"):
            if getattr(args, field) is None:
                parser.error(f"--{field.replace('_', '-')} is required")
        return access(args)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"blind access failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
