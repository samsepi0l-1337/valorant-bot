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
systemctl stop valorant-captcha-display 2>/dev/null || true
systemctl disable valorant-captcha-display 2>/dev/null || true
rm -f /etc/systemd/system/valorant-bot.service
rm -f /etc/systemd/system/valorant-bot.service.d/remote-captcha.conf
rmdir /etc/systemd/system/valorant-bot.service.d 2>/dev/null || true
rm -f /etc/systemd/system/valorant-captcha-display.service
rm -f /etc/valorant-bot/remote-captcha.conf
rm -f /etc/tmpfiles.d/valorant-captcha-display.conf
rmdir /run/valorant-captcha-display 2>/dev/null || true
rm -f /usr/local/bin/valorant-bot
systemctl daemon-reload

if [[ "$PURGE" -eq 1 ]]; then
  rm -f /var/lib/valorant-bot/data/bot.db
  rmdir /var/lib/valorant-bot/data 2>/dev/null || true
  rmdir /var/lib/valorant-bot 2>/dev/null || true
  rm -f /etc/valorant-bot/env
  rmdir /etc/valorant-bot 2>/dev/null || true
  echo "purged deployment-owned binary, service, database, and env files (kept valorant user/group)"
else
  echo "removed binary and service (kept /var/lib/valorant-bot and /etc/valorant-bot)"
  echo "full wipe: sudo $0 --purge"
fi
