#!/usr/bin/env python3
"""Check that relative links between Markdown files resolve.

GitHub resolves a relative link against the directory holding the file, so a
link written from the repository root breaks once the text is moved into a
subdirectory. Release notes are assembled by copying `release-note` blocks into
`changelog/<tag>.md`, which is exactly that move, so those links are checked
here rather than left to be found in the rendered page.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

# An inline link or image destination. Angle-bracket destinations and titles are
# handled separately so a title's parentheses are not read as the destination.
LINK = re.compile(r"!?\[[^\]]*\]\(\s*(<[^>]*>|[^\s)]+)(?:\s+[\"'(][^)]*)?\s*\)")
HEADING = re.compile(r"^ {0,3}#{1,6}\s+(.*?)\s*#*\s*$", re.MULTILINE)
HTML_ANCHOR = re.compile(r"<a\s+[^>]*(?:id|name)\s*=\s*[\"']([^\"']+)[\"']", re.IGNORECASE)
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,}).*?^ {0,3}\1", re.MULTILINE | re.DOTALL)
INLINE_CODE = re.compile(r"`+([^`]*)`+")
MD_LINK_TEXT = re.compile(r"\[([^\]]*)\]\([^)]*\)")
SKIP_DIRS = frozenset({".git", "node_modules", "dist", "build", "vendor", "coverage"})
EXTERNAL = ("http://", "https://", "mailto:", "tel:", "ftp://", "//")


def markdown_files(root: Path) -> list[Path]:
    return sorted(
        path
        for path in root.rglob("*.md")
        if not SKIP_DIRS.intersection(path.relative_to(root).parts)
    )


def slug(heading: str) -> str:
    """Return GitHub's anchor slug for a heading."""
    text = INLINE_CODE.sub(r"\1", heading)
    text = MD_LINK_TEXT.sub(r"\1", text)
    text = re.sub(r"[*_~]", "", text)
    text = re.sub(r"[^\w\- ]", "", text, flags=re.UNICODE)
    return text.strip().lower().replace(" ", "-")


def anchors(text: str) -> set[str]:
    """Return every anchor a Markdown document exposes."""
    body = FENCE.sub("", text)
    found: set[str] = set(HTML_ANCHOR.findall(text))
    seen: dict[str, int] = {}
    for heading in HEADING.findall(body):
        base = slug(heading)
        if not base:
            continue
        # GitHub disambiguates repeated headings with a -1, -2, ... suffix.
        found.add(base if base not in seen else f"{base}-{seen[base]}")
        seen[base] = seen.get(base, 0) + 1
    return found


def links(text: str) -> list[str]:
    """Return every inline link and image destination in a document."""
    return [match.group(1).strip("<>") for match in LINK.finditer(FENCE.sub("", text))]


def check(root: Path) -> list[str]:
    errors: list[str] = []
    cache: dict[Path, set[str]] = {}
    for path in markdown_files(root):
        text = path.read_text(encoding="utf-8", errors="replace")
        for destination in links(text):
            if destination.startswith(EXTERNAL):
                continue
            target, _, anchor = destination.partition("#")
            name = path.relative_to(root)
            if target:
                resolved = (path.parent / target).resolve()
                if not resolved.exists():
                    errors.append(f"{name}: {destination} does not resolve")
                    continue
                if resolved.is_dir() or resolved.suffix != ".md":
                    continue
            else:
                resolved = path.resolve()
            if not anchor:
                continue
            if resolved not in cache:
                cache[resolved] = anchors(
                    resolved.read_text(encoding="utf-8", errors="replace")
                )
            if anchor.lower() not in cache[resolved]:
                errors.append(f"{name}: {destination} has no matching heading")
    return errors


def self_test() -> None:
    import tempfile

    cases: tuple[tuple[str, dict[str, str], int], ...] = (
        ("resolving sibling link", {"a.md": "[b](b.md)", "b.md": "# B"}, 0),
        ("root-relative link from a subdirectory", {"sub/a.md": "[d](docs/b.md)", "docs/b.md": "# B"}, 1),
        ("parent-relative link from a subdirectory", {"sub/a.md": "[d](../docs/b.md)", "docs/b.md": "# B"}, 0),
        ("missing target", {"a.md": "[gone](nope.md)"}, 1),
        ("external link ignored", {"a.md": "[x](https://example.com/nope.md)"}, 0),
        ("protocol-relative link ignored", {"a.md": "[x](//example.com/nope.md)"}, 0),
        ("resolving anchor", {"a.md": "[x](b.md#some-heading)", "b.md": "## Some Heading"}, 0),
        ("missing anchor", {"a.md": "[x](b.md#absent)", "b.md": "## Some Heading"}, 1),
        ("same-file anchor", {"a.md": "# Top\n[x](#top)"}, 0),
        ("missing same-file anchor", {"a.md": "# Top\n[x](#nope)"}, 1),
        ("anchor with code span heading", {"a.md": "[x](b.md#the-value)", "b.md": "## The `value`"}, 0),
        ("duplicate headings", {"a.md": "[x](b.md#dup-1)", "b.md": "## Dup\n## Dup"}, 0),
        ("html anchor target", {"a.md": "[x](b.md#manual)", "b.md": '<a id="manual"></a>'}, 0),
        ("link inside a fenced block ignored", {"a.md": "```\n[x](nope.md)\n```"}, 0),
        ("heading inside a fenced block is not an anchor", {"a.md": "[x](b.md#fake)", "b.md": "```\n## Fake\n```"}, 1),
        ("image destination", {"a.md": "![alt](img.svg)", "img.svg": "<svg/>"}, 0),
        ("missing image destination", {"a.md": "![alt](gone.svg)"}, 1),
        ("link with a title", {"a.md": '[x](b.md "the title")', "b.md": "# B"}, 0),
        ("angle-bracket destination", {"a.md": "[x](<b.md>)", "b.md": "# B"}, 0),
        ("directory target", {"a.md": "[x](sub)", "sub/b.md": "# B"}, 0),
        ("non-markdown target skips anchors", {"a.md": "[x](s.yaml#L3)", "s.yaml": "k: v"}, 0),
        ("uppercase anchor", {"a.md": "[x](b.md#Some-Heading)", "b.md": "## Some Heading"}, 0),
        ("skipped directory", {"node_modules/a.md": "[x](nope.md)"}, 0),
    )

    for name, files, expected in cases:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for relative, content in files.items():
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content)
            errors = check(root)
            if len(errors) != expected:
                raise AssertionError(f"{name}: expected {expected} error(s), got {errors}")

    print(f"{len(cases)} documentation link scenarios passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()

    if args.self_test:
        self_test()
        return 0

    root = Path(__file__).resolve().parent.parent
    errors = check(root)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print(f"documentation links resolve across {len(markdown_files(root))} files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
