#!/usr/bin/env python3
"""Check relative Markdown links in tracked files or an explicit selection.

GitHub resolves a relative link against the directory holding the file, so a
link written from the repository root breaks once the text is moved into a
subdirectory. Release notes are assembled by copying `release-note` blocks into
`changelog/<tag>.md`, which is exactly that move, so those links are checked
here rather than left to be found in the rendered page.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

# An inline link or image destination. Angle-bracket destinations and titles are
# handled separately so a title's parentheses are not read as the destination.
# A destination may carry one level of balanced parentheses so a filename like
# API_(v2).md is read whole rather than truncated at its first bracket.
LINK = re.compile(
    r"!?\[[^\]]*\]\(\s*(<[^>]*>|(?:[^\s()]|\([^\s()]*\))+)(?:\s+[\"'(][^)]*)?\s*\)"
)
REFERENCE = re.compile(
    r"^ {0,3}\[[^\]\n]+\]:[ \t]*(<[^>\n]*>|\S+)", re.MULTILINE
)
HEADING = re.compile(r"^ {0,3}#{1,6}\s+(.*?)\s*#*\s*$", re.MULTILINE)
HTML_ANCHOR = re.compile(r"<a\s+[^>]*(?:id|name)\s*=\s*[\"']([^\"']+)[\"']", re.IGNORECASE)
HTML_TAG = re.compile(r"</?[A-Za-z][A-Za-z0-9-]*(?:\s+[^<>]*)?\s*/?>")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})[ \t]*(.*)$")
INLINE_CODE = re.compile(r"`+([^`]*)`+")
MD_LINK_TEXT = re.compile(r"\[([^\]]*)\]\([^)]*\)")
EXTERNAL = re.compile(r"^(?:[A-Za-z][A-Za-z0-9+.-]*:|//)")


def markdown_files(root: Path) -> list[Path]:
    base = root.resolve()
    repository = Path(
        subprocess.run(
            ["git", "-C", str(root), "rev-parse", "--show-toplevel"],
            check=True,
            text=True,
            stdout=subprocess.PIPE,
        ).stdout.strip()
    )
    output = subprocess.run(
        ["git", "-C", str(root), "ls-files", "--full-name", "-z", "--", "*.md"],
        check=True,
        stdout=subprocess.PIPE,
    ).stdout
    return sorted(
        path
        for relative in output.split(b"\0")
        if relative
        for path in [repository / relative.decode("utf-8")]
        if path.is_file() and path.resolve().is_relative_to(base)
    )


def strip_fences(text: str) -> str:
    """Return text with fenced code blocks removed.

    A closing fence carries no info string and is at least as long as the one
    that opened the block; an unclosed block runs to the end of the document.
    """
    kept: list[str] = []
    marker: tuple[str, int] | None = None
    for line in text.splitlines():
        match = FENCE.match(line)
        if marker is None:
            if match is not None:
                ticks, info = match.group(1), match.group(2)
                # A backtick fence's info string may not itself contain one.
                if not (ticks[0] == "`" and "`" in info):
                    marker = (ticks[0], len(ticks))
                    continue
            kept.append(line)
        elif (
            match is not None
            and match.group(1)[0] == marker[0]
            and len(match.group(1)) >= marker[1]
            and not match.group(2).strip()
        ):
            marker = None
    return "\n".join(kept)


def slug(heading: str) -> str:
    """Return GitHub's anchor slug for a heading."""
    text = "".join(
        part if index % 2 else HTML_TAG.sub("", part)
        for index, part in enumerate(INLINE_CODE.split(heading))
    )
    text = MD_LINK_TEXT.sub(r"\1", text)
    text = re.sub(r"[*_~]", "", text)
    text = re.sub(r"[^\w\- ]", "", text, flags=re.UNICODE)
    return text.strip().lower().replace(" ", "-")


def anchors(text: str) -> set[str]:
    """Return every anchor a Markdown document exposes."""
    body = strip_fences(text)
    found: set[str] = set()
    for heading in HEADING.findall(body):
        base = slug(heading)
        if not base:
            continue
        # GitHub disambiguates a repeated slug with the first free -1, -2, ...
        candidate, index = base, 0
        while candidate in found:
            index += 1
            candidate = f"{base}-{index}"
        found.add(candidate)
    return found | set(HTML_ANCHOR.findall(body))


def links(text: str) -> list[str]:
    """Return inline, image, and reference-definition destinations."""
    body = strip_fences(text)
    return [
        match.group(1).strip("<>")
        for pattern in (LINK, REFERENCE)
        for match in pattern.finditer(body)
    ]


def check(root: Path, paths: list[Path] | None = None) -> list[str]:
    """Check tracked files by default, or explicit paths relative to root."""
    selected_root = root.absolute()
    root = root.resolve()
    errors: list[str] = []
    cache: dict[Path, set[str]] = {}
    sources = markdown_files(root) if paths is None else paths
    for source in sources:
        selected = source.relative_to(selected_root) if source.is_relative_to(selected_root) else source
        path = root / selected
        if not path.is_relative_to(root) or not path.resolve().is_relative_to(root):
            errors.append(f"{source}: Markdown source leaves the root")
            continue
        if path.is_symlink() or any(
            parent.is_symlink() for parent in path.parents if parent.is_relative_to(root)
        ):
            errors.append(f"{source}: Markdown source is a symlink")
            continue
        if not path.is_file():
            errors.append(f"{source}: Markdown source is not a regular file")
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        for destination in links(text):
            if EXTERNAL.match(destination):
                continue
            target, _, anchor = destination.partition("#")
            name = path.relative_to(root)
            if target:
                resolved = (path.parent / target).resolve()
                if not resolved.is_relative_to(root):
                    errors.append(f"{name}: {destination} leaves the repository")
                    continue
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
    from unittest.mock import patch

    cases: tuple[
        tuple[str, dict[str, str], int] | tuple[str, dict[str, str], int, str], ...
    ] = (
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
        ("info string does not close a fence", {"a.md": "```py\n[x](nope.md)\n```not-a-close\n[y](gone.md)\n```"}, 0),
        ("unclosed fence runs to the end", {"a.md": "text\n```\n[x](nope.md)\n"}, 0),
        ("tilde fence", {"a.md": "~~~\n[x](nope.md)\n~~~"}, 0),
        ("longer closing fence", {"a.md": "```\n[x](nope.md)\n````"}, 0),
        ("shorter fence does not close", {"a.md": "````\n[x](nope.md)\n```\n[y](gone.md)\n````"}, 0),
        ("html anchor inside a fence is not an anchor", {"a.md": "[x](b.md#faked)", "b.md": '```\n<a id="faked"></a>\n```'}, 1),
        ("third duplicate heading", {"a.md": "[x](b.md#dup-2)", "b.md": "## Dup\n## Dup-1\n## Dup"}, 0),
        ("parenthesized destination", {"a.md": "[x](docs/API_(v2).md)", "docs/API_(v2).md": "# API"}, 0),
        ("destination outside the repository", {"repo/a.md": "[x](../outside.md)", "outside.md": "# Outside"}, 1, "repo"),
        ("image destination", {"a.md": "![alt](img.svg)", "img.svg": "<svg/>"}, 0),
        ("missing image destination", {"a.md": "![alt](gone.svg)"}, 1),
        ("link with a title", {"a.md": '[x](b.md "the title")', "b.md": "# B"}, 0),
        ("angle-bracket destination", {"a.md": "[x](<b.md>)", "b.md": "# B"}, 0),
        ("directory target", {"a.md": "[x](sub)", "sub/b.md": "# B"}, 0),
        ("non-markdown target skips anchors", {"a.md": "[x](s.yaml#L3)", "s.yaml": "k: v"}, 0),
        ("uppercase anchor", {"a.md": "[x](b.md#Some-Heading)", "b.md": "## Some Heading"}, 0),
        (
            "skipped directory",
            {
                ".gitignore": "frontend/node_modules/\n",
                "frontend/node_modules/a.md": "[x](nope.md)",
            },
            0,
        ),
        ("reference definition", {"a.md": "[b][target]\n\n[target]: b.md#b", "b.md": "# B"}, 0),
        ("missing reference target", {"a.md": "[missing][target]\n\n[target]: missing.md"}, 1),
        ("missing reference anchor", {"a.md": "[target]: b.md#absent", "b.md": "# B"}, 1),
        ("same-file reference", {"a.md": "# Top\n[target]: #top"}, 0),
        ("angle-bracket reference with title", {"a.md": '[target]: <b.md#b> "Title"', "b.md": "# B"}, 0),
        ("reference inside a fence ignored", {"a.md": "```\n[target]: missing.md\n```"}, 0),
        ("reference after false closing fence ignored", {"a.md": "```md\n```not-a-close\n[target]: missing.md\n```"}, 0),
        ("heading after false closing fence is not an anchor", {"a.md": "[x](b.md#fake)", "b.md": "```md\n```not-a-close\n## Fake\n```"}, 1),
        ("heading with HTML markup", {"a.md": "[x](b.md#some-heading)", "b.md": "## <em>Some</em> Heading"}, 0),
        ("heading with literal HTML in code", {"a.md": "[x](b.md#the-value)", "b.md": "## The `<value>`"}, 0),
        ("heading with an autolink", {"a.md": "[x](b.md#httpsexamplecom)", "b.md": "## <https://example.com>"}, 0),
        ("general external URI schemes", {"a.md": "[x](HTTPS://example.com/docs)\n[x](gs://bucket/file)\n[x](ssh://example.com)\n[x](custom+v1.2://example.com)"}, 0),
        ("external reference schemes", {"a.md": "[x]: HTTPS://example.com/docs\n[y]: gs://bucket/file\n[z]: //example.com/docs\n[email]: mailto:maintainers@example.com"}, 0),
        (
            "consumer reference and titled-link forms",
            {
                "target.md": "# Target\n",
                "a.md": """# Fixture

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
""",
            },
            0,
        ),
    )

    for name, files, expected, *scope in cases:
        with tempfile.TemporaryDirectory(prefix=".doc-links-", dir=Path.cwd()) as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "--quiet", str(root)], check=True)
            for relative, content in files.items():
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content)
            subprocess.run(["git", "-C", str(root), "add", "--all"], check=True)
            errors = check(root / scope[0] if scope else root)
            if len(errors) != expected:
                raise AssertionError(f"{name}: expected {expected} error(s), got {errors}")

    count = len(cases)
    with tempfile.TemporaryDirectory(prefix=".doc-links-", dir=Path.cwd()) as directory:
        root = Path(directory)
        subprocess.run(["git", "init", "--quiet", str(root)], check=True)
        (root / "tracked.md").write_text("# Tracked")
        subprocess.run(["git", "-C", str(root), "add", "tracked.md"], check=True)
        (root / "untracked.md").write_text("[missing](missing.md)")
        errors = check(root)
        if errors:
            raise AssertionError(f"untracked Markdown ignored: got {errors}")
        count += 1

    with tempfile.TemporaryDirectory(prefix=".doc-links-", dir=Path.cwd()) as directory:
        fixture = Path(directory)
        root = fixture / "consumer"
        deploy = root / "deploy"
        deploy.mkdir(parents=True)
        readme = deploy / "README.md"
        readme.write_text("[project](../project.yaml)\n[reference](../target.md#target)")
        (root / "project.yaml").write_text("id: sample\n")
        (root / "target.md").write_text("# Target\n")
        (root / "unselected.md").write_text("[missing](missing.md)")
        alias = fixture / "consumer-alias"
        alias.symlink_to(root, target_is_directory=True)
        with patch.object(subprocess, "run", side_effect=AssertionError("explicit selection used Git")):
            for selected_root in (root, alias):
                for paths in ([Path("deploy/README.md")], [selected_root / "deploy/README.md"], []):
                    errors = check(selected_root, paths)
                    if errors:
                        raise AssertionError(f"explicit non-Git consumer selection {paths}: {errors}")
                    count += 1

        for selected_root, source in ((root, Path("deploy/README.md")), (alias, alias / "deploy/README.md")):
            cli = subprocess.run(
                [sys.executable, str(Path(__file__).resolve()), "--root", str(selected_root), str(source)],
                cwd=fixture, text=True, capture_output=True, check=False,
            )
            if cli.returncode or "across 1 files" not in cli.stdout:
                raise AssertionError(f"explicit consumer CLI failed: {cli.stdout}{cli.stderr}")
            count += 1

        outside = fixture / "outside.md"
        outside.write_text("# Outside\n")
        (root / "source-link.md").symlink_to(root / "target.md")
        (root / "source-directory").symlink_to(deploy, target_is_directory=True)
        for selected_root in (root, alias):
            for source, expected in (
                (Path("source-link.md"), "source is a symlink"),
                (Path("source-directory/README.md"), "source is a symlink"),
                (Path("../outside.md"), "source leaves the root"),
                (outside, "source leaves the root"),
                (Path("missing.md"), "source is not a regular file"),
                (Path("deploy"), "source is not a regular file"),
            ):
                selected = source if selected_root == root else selected_root / source
                with patch.object(Path, "read_text", side_effect=AssertionError("invalid source was read")):
                    errors = check(selected_root, [selected])
                if len(errors) != 1 or expected not in errors[0]:
                    raise AssertionError(f"source validation {selected}: {errors}")
                count += 1

        (root / "contained-target.md").symlink_to(root / "target.md")
        (root / "outside-target.md").symlink_to(outside)
        for destination, expected in (
            ("../contained-target.md#target", ""),
            ("../contained-target.md#absent", "has no matching heading"),
            ("../outside-target.md#outside", "leaves the repository"),
            ("../../outside.md", "leaves the repository"),
        ):
            readme.write_text(f"[target]({destination})")
            errors = check(root, [readme])
            if (not expected and errors) or (expected and (len(errors) != 1 or expected not in errors[0])):
                raise AssertionError(f"target validation {destination}: {errors}")
            count += 1

        readme.write_text('[missing](missing.md "Missing documentation")\n')
        errors = check(root, [readme])
        if errors != ["deploy/README.md: missing.md does not resolve"]:
            raise AssertionError(f"titled link diagnostic: {errors}")
        count += 1

    print(f"{count} documentation link scenarios passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument(
        "--root", type=Path, default=Path(__file__).resolve().parent.parent,
        help="containment root (defaults to the engine repository)",
    )
    parser.add_argument(
        "files", nargs="*", type=Path,
        help="files relative to --root, or contained absolute paths; defaults to Git-tracked Markdown",
    )
    args = parser.parse_args()

    if args.self_test:
        self_test()
        return 0

    paths = args.files if args.files else markdown_files(args.root)
    errors = check(args.root, paths)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print(f"documentation links resolve across {len(paths)} files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
