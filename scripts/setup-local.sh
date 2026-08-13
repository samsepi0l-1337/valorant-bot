#!/usr/bin/env bash
# One-shot local setup (macOS / Linux laptop / WSL).
#
#   ./scripts/setup-local.sh
#   DISCORD_TOKEN=... DISCORD_APP_ID=... ./scripts/setup-local.sh --yes
#   ./scripts/setup-local.sh --run
#
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

YES=0
DO_RUN=0

usage() {
  cat <<'EOF'
Usage: ./scripts/setup-local.sh [options]

  --yes, -y   Non-interactive (require DISCORD_TOKEN / DISCORD_APP_ID env)
  --run       Start the bot after setup
  -h, --help  Show help

Creates .env from deploy/env.local.example, generates BOT_SECRET if needed,
builds bin/valorant-bot, and optionally runs it.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes|-y) YES=1; shift ;;
    --run) DO_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

prompt() {
  local var="$1" label="$2"
  if [[ -n "${!var:-}" ]]; then
    return 0
  fi
  if [[ "$YES" -eq 1 ]]; then
    echo "missing $var (set env or omit --yes)" >&2
    exit 1
  fi
  if [[ ! -t 0 ]]; then
    echo "missing $var and stdin is not a TTY" >&2
    exit 1
  fi
  printf "%s: " "$label"
  read -r "$var"
  export "$var"
}

if [[ ! -f .env ]]; then
  cp deploy/env.local.example .env
  echo "created .env from deploy/env.local.example"
else
  echo "keeping existing .env"
fi

# shellcheck disable=SC1091
source "${ROOT}/scripts/load-dotenv.sh"
load_dotenv "${ROOT}/.env"

prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)"
prompt DISCORD_APP_ID "Discord application ID (DISCORD_APP_ID)"

if [[ -z "${BOT_SECRET:-}" || "$BOT_SECRET" == change-me* ]]; then
  BOT_SECRET="$(openssl rand -base64 32 2>/dev/null || head -c 48 /dev/urandom | base64 | tr -d '\n')"
  echo "generated BOT_SECRET"
fi

AUTH_BASE_URL="${AUTH_BASE_URL:-http://127.0.0.1:8787}"
AUTH_PORT="${AUTH_PORT:-8787}"
DATABASE_PATH="${DATABASE_PATH:-./data/bot.db}"
STORE_RESET_CRON="${STORE_RESET_CRON:-0 0 * * *}"

umask 077
cat > .env <<EOF
DISCORD_TOKEN=${DISCORD_TOKEN}
DISCORD_APP_ID=${DISCORD_APP_ID}
BOT_SECRET=${BOT_SECRET}
AUTH_BASE_URL=${AUTH_BASE_URL}
AUTH_PORT=${AUTH_PORT}
DATABASE_PATH=${DATABASE_PATH}
STORE_RESET_CRON="${STORE_RESET_CRON}"
EOF

mkdir -p data
if command -v go >/dev/null 2>&1; then
  make build
else
  echo "Go not found — place a prebuilt binary at bin/valorant-bot" >&2
  exit 1
fi

INVITE="https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
echo
echo "local setup complete"
echo "  invite: $INVITE"
echo "  /auth uses Riot Mobile QR or bot-host Chrome password captcha (no inbound auth port)"
echo "  run:    make run   or   ./scripts/setup-local.sh --run"

if [[ "$DO_RUN" -eq 1 ]]; then
  exec ./scripts/run-local.sh
fi
