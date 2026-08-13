#!/usr/bin/env bash
# Run the bot locally with .env loaded.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ ! -f .env ]]; then
  echo "missing .env"
  echo "  cp deploy/env.local.example .env   # then edit secrets"
  exit 1
fi

# shellcheck disable=SC1091
source "${ROOT}/scripts/load-dotenv.sh"
load_dotenv "${ROOT}/.env"

mkdir -p "$(dirname "${DATABASE_PATH:-./data/bot.db}")"

if [[ -x ./bin/valorant-bot ]]; then
  exec ./bin/valorant-bot
fi
exec go run ./cmd/bot
