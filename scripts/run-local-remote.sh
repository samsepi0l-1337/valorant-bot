#!/usr/bin/env bash
# Start a Cloudflare quick tunnel, then the bot, so Discord CAPTCHA viewer
# links work off-LAN (LTE/mobile data). Quick tunnel URLs change every start
# and are test-only. Persistent use needs a named tunnel:
#   ./scripts/named-tunnel.sh  (https://programtyping.dreamp.org)
#   deploy/pi-cloudflare-tunnel.md
#
# Usage (repo root has .env with Discord secrets):
#   ./scripts/run-local-remote.sh
#
# Persist the quick-tunnel origin into .env (AUTH_BASE_URL, loopback bind,
# CAPTCHA_BROWSER_MODE=remote) without rewriting Discord secrets. Quick tunnel
# URLs change every start.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ ! -f .env ]]; then
  echo "missing .env"
  echo "  cp deploy/env.local.example .env   # then edit secrets"
  exit 1
fi

if ! command -v cloudflared >/dev/null 2>&1; then
  cat >&2 <<'EOF'
cloudflared not found.

Install (macOS Homebrew):
  brew install cloudflared

Or see:
  https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/
EOF
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 not found" >&2
  exit 1
fi

# shellcheck disable=SC1091
source "${ROOT}/scripts/load-dotenv.sh"
load_dotenv "${ROOT}/.env"

PORT="${AUTH_PORT:-8787}"
AUTH_BIND_ADDRESS=127.0.0.1
CAPTCHA_BROWSER_MODE=remote
export AUTH_BIND_ADDRESS CAPTCHA_BROWSER_MODE

LOG="$(mktemp)"
TUNNEL_PID=""
cleanup() {
  if [[ -n "${TUNNEL_PID}" ]]; then
    kill "${TUNNEL_PID}" 2>/dev/null || true
    wait "${TUNNEL_PID}" 2>/dev/null || true
  fi
  rm -f "${LOG}"
}
trap cleanup EXIT INT TERM

echo "Starting Cloudflare quick tunnel → http://127.0.0.1:${PORT}"
echo "Remote CAPTCHA: this quick tunnel is test-only. Named tunnels keep a stable public HTTPS AUTH_BASE_URL."
cloudflared tunnel --url "http://127.0.0.1:${PORT}" >"${LOG}" 2>&1 &
TUNNEL_PID=$!

ORIGIN=""
for _ in $(seq 1 60); do
  if ! kill -0 "${TUNNEL_PID}" 2>/dev/null; then
    echo "cloudflared exited before publishing a URL" >&2
    exit 1
  fi
  extract_status=0
  ORIGIN="$(python3 "${ROOT}/deploy/extract-trycloudflare-origin.py" <"${LOG}" 2>/dev/null)" || extract_status=$?
  if [[ "${extract_status}" -eq 0 && -n "${ORIGIN}" ]]; then
    break
  fi
  if [[ "${extract_status}" -eq 2 ]]; then
    echo "invalid Cloudflare quick tunnel origin" >&2
    exit 1
  fi
  ORIGIN=""
  sleep 1
done

if [[ -z "${ORIGIN}" ]]; then
  echo "timed out waiting for Cloudflare quick tunnel URL" >&2
  exit 1
fi

python3 "${ROOT}/deploy/validate-remote-captcha-origin.py" "${ORIGIN}"
python3 "${ROOT}/deploy/write-remote-captcha-env.py" "${ROOT}/.env" "${ORIGIN}"
AUTH_BASE_URL="$ORIGIN"
export AUTH_BASE_URL

echo "Wrote Cloudflare quick tunnel origin to .env"
echo "Cloudflare quick tunnel origin: ${AUTH_BASE_URL}"
echo "This trycloudflare URL changes on every start; do not use it as a persistent Discord link origin."
echo "Starting bot with AUTH_BIND_ADDRESS=127.0.0.1 CAPTCHA_BROWSER_MODE=remote"
echo

mkdir -p "$(dirname "${DATABASE_PATH:-./data/bot.db}")"

echo "Building bot so this run includes the latest CAPTCHA viewer fixes"
make build
if [[ -x ./bin/valorant-bot ]]; then
  ./bin/valorant-bot
else
  go run ./cmd/bot
fi
