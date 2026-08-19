#!/usr/bin/env python3
"""Validate the release-note block in a pull request body.

The block is the source of a release's notes, so it is extracted the same way
here and by the note generator: a backtick-fenced block whose info string is
exactly `release-note`. A body must carry exactly one, and it must hold either
finished prose or `NONE`.
"""

from __future__ import annotations

import argparse
import os
import re
import sys

# CommonMark reads a fence with at most three leading spaces; four makes it an
# indented code block. Tildes are deliberately not fences here, so a note may
# quote one without opening a block.
FENCE = re.compile(r"^ {0,3}(?P<ticks>`{3,})[ \t]*(?P<info>[^`]*?)[ \t]*$")
COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)

INFO_STRING = "release-note"

GUIDANCE = """
Add a release-note block to the pull request description:

    ```release-note
    What changed, and what it means for someone upgrading Aster.
    ```

Use NONE when the change has no effect a user would notice:

    ```release-note
    NONE
    ```
""".strip()


class ReleaseNoteError(Exception):
    """The body does not carry exactly one usable release-note block."""


def blocks(body: str) -> list[str]:
    """Return the contents of every top-level release-note block in body.

    Every fence is consumed, not just the release-note ones, so a fence quoted
    inside another block is part of that block rather than a second note.
    """
    found: list[str] = []
    lines = body.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    index = 0
    while index < len(lines):
        opening = FENCE.match(lines[index])
        if opening is None:
            index += 1
            continue

        ticks = opening.group("ticks")
        info = opening.group("info")
        index += 1
        collected: list[str] = []
        closed = False
        while index < len(lines):
            closing = FENCE.match(lines[index])
            if (
                closing is not None
                and not closing.group("info")
                and len(closing.group("ticks")) >= len(ticks)
            ):
                closed = True
                index += 1
                break
            collected.append(lines[index])
            index += 1

        if info != INFO_STRING:
            continue
        if not closed:
            raise ReleaseNoteError("the release-note block is missing its closing fence")
        found.append("\n".join(collected))
    return found


def note(body: str | None) -> str | None:
    """Return the release note in body, or None when it is NONE.

    Raises ReleaseNoteError when the body carries no usable block.
    """
    if not body or not body.strip():
        raise ReleaseNoteError("the pull request description is empty")

    found = blocks(body)
    if not found:
        raise ReleaseNoteError("no release-note block found")
    if len(found) > 1:
        raise ReleaseNoteError(
            f"found {len(found)} release-note blocks; the description must carry exactly one"
        )

    content = COMMENT.sub("", found[0])
    if "<!--" in content:
        # An unclosed comment would publish as an invisible note.
        raise ReleaseNoteError("the release-note block has an unclosed HTML comment")

    content = content.strip()
    if not content:
        raise ReleaseNoteError("the release-note block is empty")
    if content.upper() == "NONE":
        return None
    return content


def self_test() -> None:
    scenarios: tuple[tuple[str, str, str | object], ...] = (
        ("prose", "```release-note\nFixed a thing.\n```", "Fixed a thing."),
        ("none", "```release-note\nNONE\n```", None),
        ("lowercase none", "```release-note\nnone\n```", None),
        ("padded none", "```release-note\n\n  NONE  \n\n```", None),
        (
            "multi-paragraph prose",
            "text\n\n```release-note\nFirst.\n\nSecond.\n```\n\nmore",
            "First.\n\nSecond.",
        ),
        (
            "template comment stripped",
            "```release-note\n<!-- guidance\nspanning lines -->\nReal note.\n```",
            "Real note.",
        ),
        ("carriage returns", "```release-note\r\nFixed a thing.\r\n```", "Fixed a thing."),
        ("indented fence", "  ```release-note\n  Fixed a thing.\n  ```", "Fixed a thing."),
        (
            "longer fence",
            "````release-note\nHas an inner ``` fence.\n````",
            "Has an inner ``` fence.",
        ),
        (
            "nested code block",
            "```release-note\nUse:\n\n    make test\n```",
            "Use:\n\n    make test",
        ),
        (
            "quoted example inside a longer fence",
            "````release-note\nLike this:\n\n```release-note\nNONE\n```\n````",
            "Like this:\n\n```release-note\nNONE\n```",
        ),
    )

    failures: tuple[tuple[str, str], ...] = (
        ("empty body", ""),
        ("whitespace body", "   \n\n"),
        ("no block", "Just a description with no note."),
        ("empty block", "```release-note\n```"),
        ("whitespace block", "```release-note\n\n   \n```"),
        ("comment-only block", "```release-note\n<!-- write the note here -->\n```"),
        ("unclosed comment", "```release-note\n<!-- TODO write this\n```"),
        ("two blocks", "```release-note\nOne.\n```\n```release-note\nTwo.\n```"),
        ("unterminated block", "```release-note\nOne."),
        ("wrong info string", "```releasenote\nOne.\n```"),
        ("plain fence", "```\nOne.\n```"),
        ("tilde fence", "~~~release-note\nOne.\n~~~"),
        (
            "example quoted in a documentation block",
            "````markdown\n```release-note\nFAKE\n```\n````",
        ),
        ("indented code block", "    ```release-note\n    FAKE\n    ```"),
    )

    for name, body, expected in scenarios:
        actual = note(body)
        if actual != expected:
            raise AssertionError(f"{name}: expected {expected!r}, got {actual!r}")

    for name, body in failures:
        try:
            actual = note(body)
        except ReleaseNoteError:
            continue
        raise AssertionError(f"{name}: expected a rejection, got {actual!r}")

    print(f"{len(scenarios) + len(failures)} release-note scenarios passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument(
        "--body-env",
        help="name of the environment variable holding the pull request body",
    )
    args = parser.parse_args()

    if args.self_test:
        self_test()
        return 0

    if not args.body_env:
        parser.error("--body-env is required")

    try:
        content = note(os.environ.get(args.body_env))
    except ReleaseNoteError as error:
        print(f"release note: {error}", file=sys.stderr)
        print("", file=sys.stderr)
        print(GUIDANCE, file=sys.stderr)
        return 1

    if content is None:
        print("release note: NONE")
    else:
        print("release note:")
        print(content)
    return 0


if __name__ == "__main__":
    sys.exit(main())
