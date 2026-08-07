#!/usr/bin/env bash
# Optionally expose the bot's AUTH_PORT via a Cloudflare quick tunnel.
# Password CAPTCHA does not need this tunnel: the Discord button asks the bot
# process to open Chrome locally. Use a tunnel only for a public /invite page
# or other AUTH_PORT helpers.
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
echo "Optional public /invite URL: copy the https://….trycloudflare.com URL into AUTH_BASE_URL and restart the bot."
echo

exec cloudflared tunnel --url "http://127.0.0.1:${PORT}"
