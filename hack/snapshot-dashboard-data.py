#!/usr/bin/env python3
"""Mirror the public JSON contract of a deployed Aster site into a local directory.

The dashboard's /data tree is public, so a deployed site is the quickest source
of realistic content for frontend work: it carries published AI analyses,
recurring patterns, and pull request triage that a local fetch without AI
credentials cannot produce. Private operational files (the AI cache, traces,
usage ledgers, and write-automation state) are never served over /data and are
not mirrored here; the mock server synthesizes those instead.

Re-running replaces the snapshot rather than merging into it: a file the site no
longer publishes is deleted, and job and pull request files the current indexes
do not reference are pruned, so a snapshot never mixes two sites.

Usage:
    hack/snapshot-dashboard-data.py <site-url> [--out frontend/public/data]
"""

from __future__ import annotations

import argparse
import base64
import gzip
import json
import os
import sys
import tempfile
import urllib.error
import urllib.request
import zlib
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

# Files the SPA reads directly. dashboard.json and manifest.json are required;
# the rest depend on which features a project enabled.
ROOT_FILES = [
    ("manifest.json", True),
    ("dashboard.json", True),
    ("flakiness.json", False),
    ("search-index.json", False),
    ("resolved.json", False),
    ("pull-requests.json", False),
    ("pull-request-failures.json", False),
]

REQUEST_TIMEOUT = 60
MAX_WORKERS = 8


class Missing(Exception):
    """The site does not publish this file."""


def fetch(url: str) -> bytes:
    request = urllib.request.Request(
        url, headers={"Accept-Encoding": "gzip, deflate", "User-Agent": "aster-snapshot"}
    )
    try:
        with urllib.request.urlopen(request, timeout=REQUEST_TIMEOUT) as response:
            body = response.read()
            encoding = (response.headers.get("Content-Encoding") or "").lower()
    except urllib.error.HTTPError as err:
        if err.code == 404:
            raise Missing(url) from err
        raise RuntimeError(f"{url}: HTTP {err.code}") from err
    except urllib.error.URLError as err:
        raise RuntimeError(f"{url}: {err.reason}") from err
    if encoding == "gzip":
        return gzip.decompress(body)
    if encoding == "deflate":
        return zlib.decompress(body, -zlib.MAX_WBITS)
    return body


def write_atomic(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, tmp = tempfile.mkstemp(dir=path.parent, suffix=".tmp")
    try:
        with os.fdopen(handle, "wb") as out:
            out.write(data)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


def download(base: str, name: str, out: Path) -> int:
    """Fetch one path and write it under out. Returns bytes written, -1 if absent.

    A file the site no longer publishes is removed rather than left behind, so a
    snapshot never mixes the current site with whatever was mirrored before it.
    """
    try:
        data = fetch(f"{base}/data/{name}")
    except Missing:
        (out / name).unlink(missing_ok=True)
        return -1
    json.loads(data)  # reject an SPA fallback page served in place of JSON
    write_atomic(out / name, data)
    return len(data)


def prune(out: Path, subdir: str, keep: set[str]) -> int:
    """Remove files under out/subdir the current site does not reference."""
    directory = out / subdir
    if not directory.is_dir():
        return 0
    removed = 0
    for path in directory.iterdir():
        if path.is_file() and path.name not in keep:
            path.unlink()
            removed += 1
    return removed


def job_data_filename(job_id: str) -> str:
    """Mirror models.JobDataFilename: unpadded base64url of the job ID."""
    encoded = base64.urlsafe_b64encode(job_id.encode("utf-8")).decode("ascii")
    return f"{encoded.rstrip('=')}.json"


def child_paths(out: Path) -> list[str]:
    """Per-job and per-pull-request files referenced by the indexes just written."""
    paths = []
    dashboard = json.loads((out / "dashboard.json").read_text())
    for job in dashboard.get("jobs") or []:
        job_id = job.get("job_id")
        if job_id:
            paths.append(f"jobs/{job_data_filename(job_id)}")
    index = out / "pull-requests.json"
    if index.exists():
        for pull in json.loads(index.read_text()).get("pull_requests") or []:
            number = pull.get("number")
            if isinstance(number, int) and number > 0:
                paths.append(f"pull-requests/{number}.json")
    return paths


def download_all(base: str, names: list[str], out: Path) -> tuple[list[str], int]:
    """Fetch every child path. Returns the ones the site did not publish."""
    with ThreadPoolExecutor(max_workers=MAX_WORKERS) as pool:
        sizes = list(pool.map(lambda name: download(base, name, out), names))
    missing = [name for name, size in zip(names, sizes) if size < 0]
    return missing, sum(size for size in sizes if size > 0)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("url", help="base URL of a deployed Aster site")
    parser.add_argument(
        "--out",
        default="frontend/public/data",
        help="directory to write the snapshot into (default: %(default)s)",
    )
    args = parser.parse_args()

    base = args.url.rstrip("/")
    if not base.startswith(("http://", "https://")):
        print(f"error: {args.url} is not an http(s) URL", file=sys.stderr)
        return 2
    out = Path(args.out)

    total = 0
    for name, required in ROOT_FILES:
        try:
            size = download(base, name, out)
        except RuntimeError as err:
            print(f"error: {err}", file=sys.stderr)
            return 1
        if size < 0:
            if required:
                print(f"error: {base}/data/{name} is not published", file=sys.stderr)
                return 1
            print(f"  skip {name} (not published)")
            continue
        total += size
        print(f"  {name} ({size} bytes)")

    children = child_paths(out)
    try:
        missing, size = download_all(base, children, out)
    except RuntimeError as err:
        print(f"error: {err}", file=sys.stderr)
        return 1
    if missing:
        # A job or pull request the indexes name but the site does not serve
        # would leave a page that cannot load, so this is a failed snapshot
        # rather than a partial one.
        print(f"error: {len(missing)} referenced files are not published:", file=sys.stderr)
        for name in missing[:5]:
            print(f"  {name}", file=sys.stderr)
        return 1
    total += size
    print(f"  {len(children)} job and pull request files ({size} bytes)")

    referenced: dict[str, set[str]] = {"jobs": set(), "pull-requests": set()}
    for name in children:
        subdir, _, basename = name.partition("/")
        referenced[subdir].add(basename)
    removed = sum(prune(out, subdir, keep) for subdir, keep in referenced.items())
    if removed:
        print(f"  removed {removed} files the site no longer references")
    print(f"snapshot written to {out} ({total} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
