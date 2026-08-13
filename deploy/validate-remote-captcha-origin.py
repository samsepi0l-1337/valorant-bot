#!/usr/bin/env python3
"""Validate the install-time AUTH_BASE_URL contract for remote CAPTCHA."""

from __future__ import annotations

import ipaddress
import os
import sys
import urllib.parse


def invalid() -> int:
    print(
        "remote CAPTCHA requires AUTH_BASE_URL to be one absolute HTTPS origin "
        "or a private/local HTTP origin with a nonempty host and no userinfo, "
        "query, fragment, or non-root path",
        file=sys.stderr,
    )
    return 1


def target_invalid() -> int:
    print("remote CAPTCHA target preflight failed", file=sys.stderr)
    return 1


def looks_like_noncanonical_ipv4(hostname: str) -> bool:
    parts = hostname.removesuffix(".").split(".")
    if not 1 <= len(parts) <= 4 or any(not part for part in parts):
        return False
    for part in parts:
        digits = part[2:] if part.lower().startswith("0x") else part
        base = 16 if part.lower().startswith("0x") else 10
        if not digits:
            return False
        allowed = "0123456789abcdefABCDEF" if base == 16 else "0123456789"
        if any(character not in allowed for character in digits):
            return False
    return True


def valid_dns_name(hostname: str) -> bool:
    name = hostname.removesuffix(".")
    if not name or len(name) > 253:
        return False
    for label in name.split("."):
        if not label or len(label) > 63 or label[0] == "-" or label[-1] == "-":
            return False
        if any(not (character.isascii() and (character.isalnum() or character == "-")) for character in label):
            return False
    return True


def allowed_http_host(hostname: str) -> bool:
    if hostname.lower() == "localhost":
        return True
    try:
        address = ipaddress.ip_address(hostname)
    except ValueError:
        if not valid_dns_name(hostname):
            return False
        lower = hostname.lower()
        return lower.endswith(".local") or lower.endswith(".internal")
    return bool(address.is_private or address.is_loopback or address.is_link_local)


def valid_origin(raw: str) -> bool:
    if not raw or any(character.isspace() or ord(character) < 0x20 or ord(character) == 0x7F for character in raw):
        return False
    if "?" in raw or "#" in raw:
        return False

    try:
        parsed = urllib.parse.urlsplit(raw)
        hostname = parsed.hostname
        port = parsed.port
    except ValueError:
        return False

    scheme = parsed.scheme.lower()
    if (
        scheme not in ("https", "http")
        or not parsed.netloc
        or not hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
    ):
        return False
    if port is not None and not 1 <= port <= 65535:
        return False

    if parsed.netloc.startswith("["):
        close = parsed.netloc.find("]")
        remainder = parsed.netloc[close + 1 :]
        if close <= 1 or (remainder and (not remainder.startswith(":") or not remainder[1:].isdigit())):
            return False
        try:
            address = ipaddress.IPv6Address(hostname)
        except ValueError:
            return False
        if address.ipv4_mapped is not None or address.scope_id is not None:
            return False
        return scheme == "https" or allowed_http_host(hostname)

    if any(character in parsed.netloc for character in "[]") or ":" in hostname:
        return False
    if parsed.netloc.endswith(":"):
        return False
    try:
        address = ipaddress.ip_address(hostname)
    except ValueError:
        if looks_like_noncanonical_ipv4(hostname):
            return False
        if not valid_dns_name(hostname):
            return False
        return scheme == "https" or allowed_http_host(hostname)
    if not (isinstance(address, ipaddress.IPv4Address) and str(address) == hostname):
        return False
    return scheme == "https" or allowed_http_host(hostname)


def validate_target_env(env_path: str, caller: str) -> bool:
    if not valid_origin(caller):
        return False
    if not os.path.exists(env_path):
        return True
    try:
        with open(env_path, encoding="utf-8") as env_file:
            values = []
            for raw_line in env_file:
                line = raw_line.lstrip()
                if line.startswith("AUTH_BASE_URL="):
                    values.append(line.removeprefix("AUTH_BASE_URL=").rstrip("\n"))
    except (OSError, UnicodeError):
        return False
    return len(values) == 1 and valid_origin(values[0]) and values[0] == caller


def main() -> int:
    if len(sys.argv) == 4 and sys.argv[1] == "--target-env":
        if not validate_target_env(sys.argv[2], sys.argv[3]):
            return target_invalid()
        return 0
    if len(sys.argv) != 2 or not valid_origin(sys.argv[1]):
        return invalid()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
