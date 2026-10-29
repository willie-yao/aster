#!/usr/bin/env python3
"""Read a single-line branding.base_path for the Pages workflow."""

import pathlib
import re
import sys


def decode_scalar(text):
    if text.startswith("'"):
        match = re.fullmatch(r"'((?:[^']|'')*)'[ \t]*(?:#.*)?", text)
        if not match:
            raise ValueError("invalid single-quoted path")
        value = match[1].replace("''", "'")
    elif text.startswith('"'):
        match = re.fullmatch(r'"((?:[^"\\]|\\.)*)"[ \t]*(?:#.*)?', text)
        if not match:
            raise ValueError("invalid double-quoted path")
        escapes = {
            "0": "\0", "a": "\a", "b": "\b", "t": "\t", "\t": "\t",
            "n": "\n", "v": "\v", "f": "\f", "r": "\r", "e": "\x1b",
            " ": " ", '"': '"', "/": "/", "\\": "\\", "N": "\x85",
            "_": "\xa0", "L": "\u2028", "P": "\u2029",
        }

        def unescape(match):
            token = match[1]
            if token[0] in "xuU" and len(token) > 1:
                return chr(int(token[1:], 16))
            if token not in escapes:
                raise ValueError(f"invalid path escape: \\{token}")
            return escapes[token]

        value = re.sub(r"\\(x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|.)", unescape, match[1])
    else:
        value = re.split(r"(?:^|[ \t]+)#", text, maxsplit=1)[0].rstrip(" \t")
        if value in ("~", "null", "Null", "NULL"):
            value = ""
        if value.startswith(("|", ">")):
            raise ValueError("base path must be a single-line scalar")
    if any(char in value for char in "\0\r\n"):
        raise ValueError("base path must not contain NUL or line breaks")
    return value if value.strip() else ""


def read_base_path(path):
    in_branding = False
    field_indent = None
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith((" ", "\t")):
            if in_branding:
                break
            in_branding = re.fullmatch(r"branding:[ \t]*(?:#.*)?", line) is not None
            continue
        if not in_branding:
            continue
        indent = len(line) - len(line.lstrip(" "))
        if field_indent is None:
            field_indent = indent
        if indent != field_indent:
            continue
        match = re.fullmatch(r" +base_path:[ \t]*(.*)", line)
        if match:
            return decode_scalar(match[1])
    return ""


if __name__ == "__main__":
    config = pathlib.Path(sys.argv[1])
    try:
        sys.stdout.write(read_base_path(config))
    except (OSError, ValueError) as error:
        sys.exit(f"::error::Cannot read branding.base_path in {config}: {error}")
