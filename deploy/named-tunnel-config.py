#!/usr/bin/env python3
"""Generate and validate locally-managed Cloudflare named-tunnel config.

Credentials JSON and cert.pem stay outside the repo. This helper only writes
hostname → http://127.0.0.1:AUTH_PORT ingress with WebSocket-capable HTTP.
"""

from __future__ import annotations

import importlib.util
import ipaddress
import json
import os
import re
import sys
import tempfile
import uuid
from pathlib import Path

_SPEC = importlib.util.spec_from_file_location(
    "validate_remote_captcha_origin",
    Path(__file__).with_name("validate-remote-captcha-origin.py"),
)
if _SPEC is None or _SPEC.loader is None:
    raise SystemExit(1)
_VALIDATOR = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_VALIDATOR)

_UUID = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
    re.IGNORECASE,
)
_TUNNEL_NAME = re.compile(r"^[a-z0-9]([a-z0-9-]{0,126}[a-z0-9])?$")
_SERVICE = re.compile(r"^http://127\.0\.0\.1:([1-9][0-9]{0,4})$")


def failed() -> int:
    print("named tunnel config failed", file=sys.stderr)
    return 1


def parse_failed() -> int:
    print("named tunnel list parse failed", file=sys.stderr)
    return 2


def yaml_quote(value: str) -> str | None:
    if not value or any(ord(character) < 0x20 or ord(character) == 0x7F for character in value):
        return None
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def valid_tunnel_name(name: str) -> bool:
    if not name or name != name.lower() or "--" in name:
        return False
    return _TUNNEL_NAME.fullmatch(name) is not None


def named_tunnel_hostname(raw: str) -> str | None:
    if not raw or any(character.isspace() or ord(character) < 0x20 or ord(character) == 0x7F for character in raw):
        return None
    host = raw.strip().lower().removesuffix(".")
    if "." not in host or not _VALIDATOR.valid_dns_name(host):
        return None
    try:
        ipaddress.ip_address(host)
        return None
    except ValueError:
        pass
    if host in ("localhost", "trycloudflare.com") or host.endswith(".trycloudflare.com") or host.endswith(".local") or host.endswith(".internal"):
        return None
    if not _VALIDATOR.valid_origin(f"https://{host}"):
        return None
    return host


def public_origin(hostname: str) -> str | None:
    host = named_tunnel_hostname(hostname)
    if host is None:
        return None
    return f"https://{host}"


def origin_service(port_raw: str) -> str | None:
    if not port_raw.isdigit():
        return None
    port = int(port_raw)
    if not 1 <= port <= 65535:
        return None
    service = f"http://127.0.0.1:{port}"
    if _SERVICE.fullmatch(service) is None:
        return None
    return service


def valid_service(service: str) -> bool:
    match = _SERVICE.fullmatch(service)
    if match is None:
        return False
    return 1 <= int(match.group(1)) <= 65535


def valid_tunnel_id(raw: str) -> str | None:
    if _UUID.fullmatch(raw) is None:
        return None
    try:
        return str(uuid.UUID(raw))
    except ValueError:
        return None


def path_under(path: str, root: str) -> bool:
    try:
        abs_path = os.path.abspath(path)
        abs_root = os.path.abspath(root)
        return os.path.commonpath([abs_path, abs_root]) == abs_root
    except (ValueError, OSError):
        return False


def valid_credentials_file(path: str, repo_root: str | None) -> bool:
    if not path or not os.path.isabs(path) or os.path.normpath(path) != os.path.abspath(path):
        return False
    if not path.endswith(".json") or os.path.basename(path) in ("", ".", ".."):
        return False
    if any(ord(character) < 0x20 or ord(character) == 0x7F or character in "\r\n" for character in path):
        return False
    if repo_root and path_under(path, repo_root):
        return False
    return True


def _is_go_zero_time(deleted: object) -> bool:
    if deleted in (None, "", False, 0):
        return True
    return str(deleted).replace(" ", "T").startswith("0001-01-01")


def tunnel_deleted(entry: dict) -> bool:
    deleted = entry.get("deletedAt", entry.get("deleted_at"))
    if _is_go_zero_time(deleted):
        return False
    return True


def parse_tunnel_id(payload: str, name: str) -> tuple[str | None, bool]:
    """Return (uuid, invalid). uuid is set only for one live tunnel with name."""
    if not valid_tunnel_name(name):
        return None, True
    try:
        data = json.loads(payload)
    except json.JSONDecodeError:
        return None, True
    if data is None:
        return None, False
    if isinstance(data, dict):
        for key in ("tunnels", "result", "success"):
            if key in data and isinstance(data[key], list):
                data = data[key]
                break
        else:
            return None, True
    if not isinstance(data, list):
        return None, True
    found: list[str] = []
    for entry in data:
        if not isinstance(entry, dict):
            return None, True
        entry_name = entry.get("name")
        if not isinstance(entry_name, str) or entry_name != name:
            continue
        if tunnel_deleted(entry):
            continue
        raw_id = entry.get("id", entry.get("ID"))
        if not isinstance(raw_id, str):
            return None, True
        tunnel_id = valid_tunnel_id(raw_id)
        if tunnel_id is None:
            return None, True
        if tunnel_id not in found:
            found.append(tunnel_id)
    if len(found) > 1:
        return None, True
    if len(found) == 1:
        return found[0], False
    return None, False


def render_config(tunnel_id: str, credentials_file: str, hostname: str, service: str) -> str | None:
    canonical_id = valid_tunnel_id(tunnel_id)
    host = named_tunnel_hostname(hostname)
    if canonical_id is None or host is None or not valid_service(service):
        return None
    quoted = {
        "tunnel": yaml_quote(canonical_id),
        "credentials": yaml_quote(credentials_file),
        "hostname": yaml_quote(host),
        "service": yaml_quote(service),
    }
    if any(value is None for value in quoted.values()):
        return None
    return (
        "# Locally-managed Cloudflare named tunnel for valorant-bot AUTH_PORT.\n"
        "# Proxies viewer HTML and WebSocket Upgrade to loopback only.\n"
        "# Do not point ingress at Riot login; captcha tokens are not minted on this hostname.\n"
        f"tunnel: {quoted['tunnel']}\n"
        f"credentials-file: {quoted['credentials']}\n"
        "ingress:\n"
        f"  - hostname: {quoted['hostname']}\n"
        f"    service: {quoted['service']}\n"
        "    originRequest:\n"
        '      connectTimeout: "30s"\n'
        '      keepAliveTimeout: "90s"\n'
        "  - service: http_status:404\n"
    )


def write_atomic(path: str, contents: str) -> None:
    directory = os.path.dirname(os.path.abspath(path)) or "."
    fd, tmp = tempfile.mkstemp(prefix=".named-tunnel.", suffix=".yml.tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
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


def parse_render_args(argv: list[str]) -> dict[str, str] | None:
    values: dict[str, str] = {}
    index = 0
    flags = {
        "--tunnel-id": "tunnel_id",
        "--credentials-file": "credentials_file",
        "--hostname": "hostname",
        "--service": "service",
        "--output": "output",
        "--repo-root": "repo_root",
    }
    while index < len(argv):
        flag = argv[index]
        if flag not in flags or index + 1 >= len(argv):
            return None
        values[flags[flag]] = argv[index + 1]
        index += 2
    required = ("tunnel_id", "credentials_file", "hostname", "service")
    if any(key not in values or not values[key] for key in required):
        return None
    return values


def cmd_origin(argv: list[str]) -> int:
    if len(argv) != 1:
        return failed()
    origin = public_origin(argv[0])
    if origin is None:
        return failed()
    print(origin)
    return 0


def cmd_service(argv: list[str]) -> int:
    if len(argv) != 1:
        return failed()
    service = origin_service(argv[0])
    if service is None:
        return failed()
    print(service)
    return 0


def cmd_tunnel_name(argv: list[str]) -> int:
    if len(argv) != 1 or not valid_tunnel_name(argv[0]):
        return failed()
    print(argv[0])
    return 0


def cmd_parse_list(argv: list[str]) -> int:
    if len(argv) != 1:
        return parse_failed()
    try:
        payload = sys.stdin.read()
    except OSError:
        return parse_failed()
    tunnel_id, invalid = parse_tunnel_id(payload, argv[0])
    if invalid:
        return parse_failed()
    if tunnel_id is None:
        return 1
    print(tunnel_id)
    return 0


def cmd_render(argv: list[str]) -> int:
    values = parse_render_args(argv)
    if values is None:
        return failed()
    repo_root = values.get("repo_root") or None
    if not valid_credentials_file(values["credentials_file"], repo_root):
        return failed()
    rendered = render_config(
        values["tunnel_id"],
        values["credentials_file"],
        values["hostname"],
        values["service"],
    )
    if rendered is None:
        return failed()
    output = values.get("output")
    if not output:
        sys.stdout.write(rendered)
        return 0
    if not os.path.isabs(output) or os.path.normpath(output) != os.path.abspath(output):
        return failed()
    if repo_root and path_under(output, repo_root):
        return failed()
    try:
        write_atomic(output, rendered)
    except OSError:
        return failed()
    return 0


def main() -> int:
    if len(sys.argv) < 2:
        return failed()
    command, rest = sys.argv[1], sys.argv[2:]
    if command == "origin":
        return cmd_origin(rest)
    if command == "service":
        return cmd_service(rest)
    if command == "tunnel-name":
        return cmd_tunnel_name(rest)
    if command == "parse-list":
        return cmd_parse_list(rest)
    if command == "render":
        return cmd_render(rest)
    return failed()


if __name__ == "__main__":
    raise SystemExit(main())
