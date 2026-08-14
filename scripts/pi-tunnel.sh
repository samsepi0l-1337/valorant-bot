#!/usr/bin/env bash
# Optionally expose the bot's loopback AUTH_PORT via a Cloudflare quick tunnel.
# In remote CAPTCHA mode this is test-only: persistent use needs a stable public
# HTTPS AUTH_BASE_URL and a named tunnel/reverse proxy with WebSocket support
# (./scripts/named-tunnel.sh, https://programtyping.dreamp.org).
# QR/disabled mode needs no inbound port or tunnel.
#
# Usage (on the Pi, while valorant-bot is already listening on AUTH_PORT):
#   ./scripts/pi-tunnel.sh
#   ./scripts/pi-tunnel.sh 8787
#
# Then:
#   1) Copy the printed https://….trycloudflare.com URL
#   2) Set AUTH_BASE_URL to that URL in .env / /etc/valorant-bot/env
#   3) Restart the bot
#
# Install cloudflared once:
#   https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/
#
set -euo pipefail

PORT="${1:-${AUTH_PORT:-8787}}"
if [[ -z "${CAPTCHA_BROWSER_MODE:-}" && -e /etc/valorant-bot/remote-captcha.conf ]]; then
  # The opt-in deployment asset exists only while the remote display drop-in is active.
  CAPTCHA_MODE=remote
else
  CAPTCHA_MODE="${CAPTCHA_BROWSER_MODE:-disabled}"
fi

if ! command -v cloudflared >/dev/null 2>&1; then
  cat >&2 <<'EOF'
cloudflared not found.

Install (Raspberry Pi OS / Debian arm64 example):
  curl -L --output cloudflared.deb \
    https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64.deb
  sudo dpkg -i cloudflared.deb

Then re-run: ./scripts/pi-tunnel.sh
EOF
  exit 1
fi

echo "Starting Cloudflare quick tunnel → http://127.0.0.1:${PORT}"
if [[ "$CAPTCHA_MODE" == "remote" ]]; then
  echo "Remote CAPTCHA mode: this quick tunnel is test-only."
  echo "Production requires a stable public HTTPS AUTH_BASE_URL and Tunnel/reverse proxy WebSocket support."
  echo "For this test, copy the printed https://….trycloudflare.com URL into AUTH_BASE_URL and restart the bot."
else
  echo "QR/disabled mode needs no inbound port or tunnel; this is optional for /invite only."
fi
echo

exec cloudflared tunnel --url "http://127.0.0.1:${PORT}"
