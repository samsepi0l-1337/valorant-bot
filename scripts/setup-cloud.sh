#!/usr/bin/env bash
# One-shot cloud / VPS setup (linux/amd64).
#
# On the server (with this repo):
#   sudo ./scripts/setup-cloud.sh
#   sudo ./scripts/setup-cloud.sh --docker
#
# From your laptop (build + install over SSH):
#   ./scripts/setup-cloud.sh --host user@vps.example.com
#   ./scripts/setup-cloud.sh --host user@vps --docker
#
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

HOST=""
USE_DOCKER=0
YES=0
SKIP_START=0

usage() {
  cat <<'EOF'
Usage: ./scripts/setup-cloud.sh [options]

  --host USER@HOST  Build linux/amd64 locally and install on remote host
  --docker          Use Docker Compose instead of systemd
  --yes, -y         Non-interactive (DISCORD_TOKEN / DISCORD_APP_ID required)
  --skip-start      Install files only; do not start the service
  -h, --help        Show help

Writes env, installs the bot (systemd or Docker), and starts it when secrets
are present. /auth needs no inbound auth port. Password login additionally
requires a desktop session with Chrome/Chromium on the bot host; otherwise use QR.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) HOST="${2:-}"; shift 2 ;;
    --docker) USE_DOCKER=1; shift ;;
    --yes|-y) YES=1; shift ;;
    --skip-start) SKIP_START=1; shift ;;
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

write_env_file() {
  local dest="$1" db_path="$2" auth_base="$3"
  local secret="${BOT_SECRET:-}"
  if [[ -z "$secret" || "$secret" == change-me* ]]; then
    secret="$(openssl rand -base64 32 2>/dev/null || head -c 48 /dev/urandom | base64 | tr -d '\n')"
  fi
  umask 077
  cat > "$dest" <<EOF
DISCORD_TOKEN=${DISCORD_TOKEN}
DISCORD_APP_ID=${DISCORD_APP_ID}
BOT_SECRET=${secret}
AUTH_BASE_URL=${auth_base}
AUTH_PORT=${AUTH_PORT:-8787}
DATABASE_PATH=${db_path}
STORE_RESET_CRON=${STORE_RESET_CRON:-0 0 * * *}
EOF
}

prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)"
prompt DISCORD_APP_ID "Discord application ID (DISCORD_APP_ID)"

if [[ -n "$HOST" ]]; then
  echo "building linux/amd64…"
  make build-linux
  REMOTE_DIR="valorant-bot-deploy"
  scp -q dist/valorant-bot-linux-amd64 "$HOST:/tmp/valorant-bot-linux-amd64"
  ssh "$HOST" "mkdir -p ~/${REMOTE_DIR}"
  scp -q -r deploy scripts Makefile go.mod go.sum cmd internal pkg "$HOST:~/${REMOTE_DIR}/" 2>/dev/null || \
    scp -q -r deploy scripts "$HOST:~/${REMOTE_DIR}/"

  ENV_TMP="$(mktemp)"
  AUTH_BASE_URL="${AUTH_BASE_URL:-https://bot.example.com}"
  write_env_file "$ENV_TMP" "/var/lib/valorant-bot/data/bot.db" "$AUTH_BASE_URL"
  scp -q "$ENV_TMP" "$HOST:/tmp/valorant.env"
  rm -f "$ENV_TMP"

  if [[ "$USE_DOCKER" -eq 1 ]]; then
    scp -q Dockerfile docker-compose.yml "$HOST:~/${REMOTE_DIR}/"
    ssh -t "$HOST" "cd ~/${REMOTE_DIR} && cp /tmp/valorant.env .env.docker && docker compose --env-file .env.docker up -d --build"
  else
    INSTALL_FLAGS=(--binary /tmp/valorant-bot-linux-amd64 --env /tmp/valorant.env)
    if [[ "$SKIP_START" -eq 1 ]]; then
      INSTALL_FLAGS+=(--skip-start)
    fi
    ssh -t "$HOST" "cd ~/${REMOTE_DIR} && sudo ./deploy/install.sh ${INSTALL_FLAGS[*]}"
  fi

  echo
  echo "cloud setup complete on $HOST"
  echo "  invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
  echo "  logs:   ssh $HOST 'sudo journalctl -u valorant-bot -f'"
  exit 0
fi

# Local / on-server path
if [[ "$USE_DOCKER" -eq 1 ]]; then
  AUTH_BASE_URL="${AUTH_BASE_URL:-http://127.0.0.1:8787}"
  write_env_file .env.docker "/var/lib/valorant-bot/data/bot.db" "$AUTH_BASE_URL"
  if [[ "$SKIP_START" -eq 1 ]]; then
    echo "wrote .env.docker — start with: docker compose --env-file .env.docker up -d --build"
    exit 0
  fi
  docker compose --env-file .env.docker up -d --build
  docker compose logs --tail=30
  echo "invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
  exit 0
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root on the server: sudo $0 …" >&2
  echo "or from a laptop: $0 --host user@server" >&2
  exit 1
fi

AUTH_BASE_URL="${AUTH_BASE_URL:-http://127.0.0.1:8787}"
ENV_TMP="$(mktemp)"
write_env_file "$ENV_TMP" "/var/lib/valorant-bot/data/bot.db" "$AUTH_BASE_URL"
INSTALL_FLAGS=(--env "$ENV_TMP")
if [[ "$SKIP_START" -eq 1 ]]; then
  INSTALL_FLAGS+=(--skip-start)
fi
./deploy/install.sh "${INSTALL_FLAGS[@]}"
rm -f "$ENV_TMP"

echo
echo "cloud setup complete"
echo "  invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
echo "  logs:   journalctl -u valorant-bot -f"
