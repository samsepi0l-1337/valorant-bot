#!/usr/bin/env python3
"""Persist a Cloudflare tunnel origin into a local env file without rewriting secrets."""

from __future__ import annotations

import importlib.util
import os
import sys
import tempfile
from pathlib import Path

_SPEC = importlib.util.spec_from_file_location(
    "validate_remote_captcha_origin",
    Path(__file__).with_name("validate-remote-captcha-origin.py"),
)
if _SPEC is None or _SPEC.loader is None:
    raise SystemExit(1)
_VALIDATOR = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_VALIDATOR)

_REMOTE_KEYS = ("AUTH_BASE_URL", "AUTH_BIND_ADDRESS", "CAPTCHA_BROWSER_MODE")


def failed() -> int:
    print("remote captcha env update failed", file=sys.stderr)
    return 1


def assignment_key(line: str) -> str | None:
    body = line[:-1] if line.endswith("\n") else line
    if body.endswith("\r"):
        body = body[:-1]
    stripped = body.lstrip()
    if not stripped or stripped.startswith("#") or "=" not in stripped:
        return None
    key = stripped.split("=", 1)[0].strip()
    return key or None


def replace_assignment(line: str, key: str, value: str) -> str:
    suffix = ""
    body = line
    if body.endswith("\n"):
        suffix = "\n"
        body = body[:-1]
        if body.endswith("\r"):
            suffix = "\r\n"
            body = body[:-1]
    indent = body[: len(body) - len(body.lstrip())]
    return f"{indent}{key}={value}{suffix}"


def update_env(contents: str, origin: str) -> str | None:
    values = {
        "AUTH_BASE_URL": origin,
        "AUTH_BIND_ADDRESS": "127.0.0.1",
        "CAPTCHA_BROWSER_MODE": "remote",
    }
    counts = {key: 0 for key in _REMOTE_KEYS}
    lines = contents.splitlines(keepends=True)
    updated: list[str] = []
    for line in lines:
        key = assignment_key(line)
        if key in values:
            counts[key] += 1
            updated.append(replace_assignment(line, key, values[key]))
            continue
        updated.append(line)
    if any(count > 1 for count in counts.values()):
        return None
    missing = [key for key in _REMOTE_KEYS if counts[key] == 0]
    if missing:
        if updated and not updated[-1].endswith(("\n", "\r")):
            updated[-1] += "\n"
        for key in missing:
            updated.append(f"{key}={values[key]}\n")
    return "".join(updated)


def write_atomic(path: str, contents: str) -> None:
    directory = os.path.dirname(os.path.abspath(path)) or "."
    fd, tmp = tempfile.mkstemp(prefix=".env.", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as handle:
            handle.write(contents)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
        tmp = ""
    finally:
        if tmp:
            try:
                os.unlink(tmp)
            except OSError:
                pass
    os.chmod(path, 0o600)


def main() -> int:
    if len(sys.argv) != 3:
        return failed()
    env_path, origin = sys.argv[1], sys.argv[2]
    if not _VALIDATOR.valid_origin(origin):
        return failed()
    if not os.path.isfile(env_path):
        return failed()
    try:
        with open(env_path, encoding="utf-8", newline="") as env_file:
            current = env_file.read()
        updated = update_env(current, origin)
        if updated is None:
            return failed()
        write_atomic(env_path, updated)
    except (OSError, UnicodeError):
        return failed()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
