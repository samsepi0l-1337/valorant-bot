#!/usr/bin/env python3
"""Extract a Cloudflare quick-tunnel origin from cloudflared logs on stdin."""

from __future__ import annotations

import importlib.util
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

_SPEC = importlib.util.spec_from_file_location(
    "validate_remote_captcha_origin",
    Path(__file__).with_name("validate-remote-captcha-origin.py"),
)
if _SPEC is None or _SPEC.loader is None:
    raise SystemExit(1)
_VALIDATOR = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_VALIDATOR)

_URL = re.compile(r"https?://[^\s\"'<>]+", re.IGNORECASE)
_TRAILING_PUNCT = ".,:;)]}"


def _hostname(raw: str) -> str | None:
    try:
        host = urlsplit(raw).hostname
    except ValueError:
        return None
    if host is None:
        return None
    return host.lower().removesuffix(".")


def _is_trycloudflare(host: str) -> bool:
    return host == "trycloudflare.com" or host.endswith(".trycloudflare.com")


def _canonical_origin(raw: str) -> str | None:
    trimmed = raw.rstrip("/")
    if not _VALIDATOR.valid_origin(trimmed) and not _VALIDATOR.valid_origin(raw):
        return None
    candidate = trimmed if _VALIDATOR.valid_origin(trimmed) else raw.rstrip("/")
    try:
        parsed = urlsplit(candidate)
        host = parsed.hostname
    except ValueError:
        return None
    if parsed.scheme.lower() != "https" or not host:
        return None
    host = host.lower().removesuffix(".")
    if parsed.port not in (None, 443):
        return f"https://{host}:{parsed.port}"
    return f"https://{host}"


def extract(text: str) -> tuple[str | None, bool]:
    """Return (origin, invalid_candidate). origin is set only when unique and valid."""
    origins: list[str] = []
    invalid = False
    for match in _URL.finditer(text):
        raw = match.group(0).rstrip(_TRAILING_PUNCT)
        host = _hostname(raw)
        if host is None or not _is_trycloudflare(host):
            continue
        if host == "trycloudflare.com":
            invalid = True
            continue
        origin = _canonical_origin(raw)
        if origin is None:
            invalid = True
            continue
        if origin not in origins:
            origins.append(origin)
    if invalid or len(origins) > 1:
        return None, True
    if len(origins) == 1:
        return origins[0], False
    return None, False


def main() -> int:
    try:
        text = sys.stdin.read()
    except OSError:
        print("no Cloudflare quick tunnel origin found", file=sys.stderr)
        return 1
    origin, invalid = extract(text)
    if invalid:
        print("invalid Cloudflare quick tunnel origin", file=sys.stderr)
        return 2
    if origin is None:
        print("no Cloudflare quick tunnel origin found", file=sys.stderr)
        return 1
    print(origin)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
