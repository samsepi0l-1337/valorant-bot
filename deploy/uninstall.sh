#!/usr/bin/env bash
# Remove systemd unit and binary (keeps data + env unless --purge).
set -euo pipefail

PURGE=0
if [[ "${1:-}" == "--purge" ]]; then
  PURGE=1
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root: sudo $0 [--purge]" >&2
  exit 1
fi

systemctl stop valorant-bot 2>/dev/null || true
systemctl disable valorant-bot 2>/dev/null || true
rm -f /etc/systemd/system/valorant-bot.service
rm -f /usr/local/bin/valorant-bot
systemctl daemon-reload

if [[ "$PURGE" -eq 1 ]]; then
  rm -rf /var/lib/valorant-bot /etc/valorant-bot
  if id -u valorant >/dev/null 2>&1; then
    userdel valorant 2>/dev/null || true
  fi
  if getent group valorant >/dev/null; then
    groupdel valorant 2>/dev/null || true
  fi
  echo "purged binary, service, data, and env"
else
  echo "removed binary and service (kept /var/lib/valorant-bot and /etc/valorant-bot)"
  echo "full wipe: sudo $0 --purge"
fi
