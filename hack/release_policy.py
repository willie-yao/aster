#!/usr/bin/env python3
"""Version ordering for main releases and maintained minor lines."""

import argparse
import re
import sys


RELEASE = re.compile(
    r"^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-(beta|rc)\.(0|[1-9][0-9]*))?$"
)
MAINTENANCE = re.compile(r"^release/(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def parse(tag):
    match = RELEASE.fullmatch(tag)
    if not match:
        raise ValueError(f"invalid release tag: {tag}")
    major, minor, patch, phase, number = match.groups()
    stage = (1, 0, 0) if phase is None else (0, {"beta": 0, "rc": 1}[phase], int(number))
    return (int(major), int(minor), int(patch), stage)


def existing_releases(tags):
    return [(tag, parse(tag)) for tag in tags if RELEASE.fullmatch(tag)]


def check_version(tag, branch, tags):
    requested = parse(tag)
    existing = existing_releases(tags)
    if branch == "main":
        candidates = existing
    else:
        match = MAINTENANCE.fullmatch(branch)
        if not match:
            raise ValueError(f"unsupported release source branch: {branch}")
        line = tuple(map(int, match.groups()))
        if requested[:2] != line or requested[2] == 0:
            raise ValueError(f"{tag} is not a patch release on {branch}")
        anchor = f"v{line[0]}.{line[1]}.0"
        if anchor not in tags:
            raise ValueError(f"{branch} requires an existing stable release tag {anchor}")
        candidates = [(name, version) for name, version in existing if version[:2] == line]

    newest = max(((version, name) for name, version in candidates if name != tag), default=None)
    if newest is not None and requested <= newest[0]:
        suffix = "" if branch == "main" else f" on {branch}"
        raise ValueError(f"refusing to publish {tag}: {newest[1]} is already released{suffix}")


def promotion_flags(tag, tags):
    requested = parse(tag)
    if requested[3][0] == 0:
        return False, False
    stable = [version for _, version in existing_releases(tags) if version[3][0] == 1]
    in_major = [version for version in stable if version[0] == requested[0]]
    return (
        not in_major or requested >= max(in_major),
        not stable or requested >= max(stable),
    )


def self_test():
    check_version("v1.4.1", "main", ["v1.4.0"])
    check_version("v1.4.1", "release/1.4", ["v1.4.0", "v1.5.0"])
    check_version("v1.4.1-rc.1", "release/1.4", ["v1.4.0", "v1.5.0"])
    check_version("v1.4.1", "release/1.4", ["v1.4.0", "v1.4.1-rc.1", "v1.5.0"])
    check_version("v1.10.0", "main", ["v1.9.5"])
    for tag, branch, tags in (
        ("v1.4.1", "main", ["v1.5.0"]),
        ("v1.4.1", "release/1.5", ["v1.4.0", "v1.5.0"]),
        ("v1.4.0", "release/1.4", ["v1.4.0"]),
        ("v1.4.1", "release/1.4", ["v1.5.0"]),
        ("v1.4.1-beta.2", "release/1.4", ["v1.4.0", "v1.4.1-rc.1"]),
        ("v1.4.1-rc.1", "release/1.4", ["v1.4.0", "v1.4.1-rc.2"]),
        ("v1.4.1-rc.1", "release/1.4", ["v1.4.0", "v1.4.1"]),
        ("v1.4.1", "release/01.4", ["v1.4.0"]),
    ):
        try:
            check_version(tag, branch, tags)
        except ValueError:
            continue
        raise AssertionError(f"accepted {tag} on {branch} with {tags}")
    assert promotion_flags("v1.4.1", ["v1.4.0", "v1.5.0"]) == (False, False)
    assert promotion_flags("v1.4.1", ["v1.4.0", "v2.0.0"]) == (True, False)
    assert promotion_flags("v1.4.1", ["v1.4.0", "v1.4.2-rc.1"]) == (True, True)
    assert promotion_flags("v1.4.1-rc.1", ["v1.4.0"]) == (False, False)
    print("Release version policy checks passed.")


def main():
    if sys.argv[1:] == ["--self-test"]:
        self_test()
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("check", "promote"))
    parser.add_argument("tag")
    parser.add_argument("--branch", default="main")
    args = parser.parse_args()
    tags = set(sys.stdin.read().splitlines())
    try:
        if args.action == "check":
            check_version(args.tag, args.branch, tags)
        else:
            major, latest = promotion_flags(args.tag, tags)
            print(f"{str(major).lower()} {str(latest).lower()}")
    except ValueError as exc:
        parser.exit(1, f"{exc}\n")


if __name__ == "__main__":
    main()
