#!/usr/bin/env bash
# Fails when the AGENTS.md "Repo layout" map and the backend tree diverge, so
# the map cannot silently rot as packages are added or removed. The map is the
# orientation contract for contributors and agents alike.
#
# Namespace-only directories are traversed. Nested helpers within a package
# (ai/tools/k8s and friends) are documented but not enforced.
set -o errexit
set -o nounset
set -o pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

python3 - "$@" <<'PY'
import argparse
import pathlib
import re
import tempfile


def package_paths(root):
    def walk(directory):
        if any(directory.glob("*.go")):
            yield directory.relative_to(root).as_posix()
            return
        for child in sorted(directory.iterdir()):
            if child.is_dir() and child.name != "testdata" and not child.name.startswith((".", "_")):
                yield from walk(child)

    return {
        package
        for area in ("backend/cmd", "backend/internal")
        for package in walk(root / area)
    }


def mapped_paths(text):
    section = text.split("## Repo layout\n", 1)[1].split("\n## ", 1)[0]
    backend = re.split(r"\n[a-z]", section.split("\nbackend/", 1)[1], maxsplit=1)[0]
    stack = [(0, "backend")]
    paths = set()
    for line in backend.splitlines():
        match = re.match(r"^( +)([a-z][a-z0-9]*(?:/[a-z][a-z0-9]*)*)/(?:\s|$)", line)
        if not match:
            continue
        indent, name = len(match[1]), match[2]
        while stack[-1][0] >= indent:
            stack.pop()
        path = stack[-1][1] + "/" + name
        stack.append((indent, path))
        if indent > 2 and path.startswith(("backend/cmd/", "backend/internal/")):
            paths.add(path)
    return paths


def differences(root, text):
    packages = package_paths(root)
    mapped = mapped_paths(text)
    # Namespace entries are valid ancestors; helpers under a package stay optional.
    valid = packages | {
        parent.as_posix()
        for package in packages
        for parent in pathlib.PurePosixPath(package).parents
    }
    stale = {
        path for path in mapped
        if path not in valid and not any(path.startswith(package + "/") for package in packages)
    }
    return packages - mapped, stale


def self_test():
    text = """## Repo layout

```
backend/
  cmd/
    server/
  internal/
    server/
    runtime/
    ai/
      tools/{filesystem,k8s,repotree}/
    pullrequest/
      triage/
    fix/
      runtime/
    prow/jobconfig/
frontend/
  src/
    components/
```

## Next section
"""
    with tempfile.TemporaryDirectory(prefix=".repo-map-test-", dir=".") as directory:
        root = pathlib.Path(directory)
        for package in (
            "cmd/server", "internal/server", "internal/runtime", "internal/ai",
            "internal/ai/tools/k8s", "internal/pullrequest/triage",
            "internal/fix/runtime", "internal/fix/runtime/testdata/fakeexecutor",
            "internal/prow/jobconfig",
        ):
            path = root / "backend" / package
            path.mkdir(parents=True, exist_ok=True)
            (path / "package.go").write_text("package fixture\n")
        scenarios = (
            ("valid namespaces and optional helpers", text, set(), set()),
            ("missing nested package", text.replace("      triage/\n", ""),
             {"backend/internal/pullrequest/triage"}, set()),
            ("stale nested package", text.replace("      triage/", "      removed/"),
             {"backend/internal/pullrequest/triage"}, {"backend/internal/pullrequest/removed"}),
            ("same leaf in another namespace", text.replace("    fix/\n      runtime/\n", ""),
             {"backend/internal/fix/runtime"}, set()),
            ("cmd ownership", text.replace("  cmd/\n    server/\n", "  cmd/\n"),
             {"backend/cmd/server"}, set()),
            ("missing top-level package", text.replace("\n    runtime/\n", "\n"),
             {"backend/internal/runtime"}, set()),
            ("stale top-level package", text.replace("    prow/jobconfig/", "    removed/"),
             {"backend/internal/prow/jobconfig"}, {"backend/internal/removed"}),
        )
        for name, candidate, missing, stale in scenarios:
            actual = differences(root, candidate)
            assert actual == (missing, stale), (name, actual)
    print(f"{len(scenarios)} repo map scenarios passed")


parser = argparse.ArgumentParser()
parser.add_argument("--self-test", action="store_true")
args = parser.parse_args()
if args.self_test:
    self_test()
else:
    missing, stale = differences(pathlib.Path("."), pathlib.Path("AGENTS.md").read_text())
    for title, paths in (
        ("AGENTS.md repo map does not list these packages:", missing),
        ("AGENTS.md repo map lists these packages, but they no longer exist:", stale),
    ):
        if paths:
            print(title)
            for path in sorted(paths):
                print(f"  {path}/")
    if missing or stale:
        print('\nUpdate the "Repo layout" section in AGENTS.md to match the tree.')
        raise SystemExit(1)
    print("AGENTS.md repo map matches backend/cmd and backend/internal.")
PY
