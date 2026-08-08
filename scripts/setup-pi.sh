#!/usr/bin/env bash
# One-shot Raspberry Pi setup.
#
# From a Mac/PC (recommended):
#   ./scripts/setup-pi.sh --host pi@192.168.0.10
#   ./scripts/setup-pi.sh --host pi@raspberrypi.local --arch armv7
#
# On the Pi itself (with this repo + Go, or a prebuilt binary):
#   sudo ./scripts/setup-pi.sh
#
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

HOST=""
ARCH="arm64"
YES=0
SKIP_START=0
REMOTE_CAPTCHA=0

usage() {
  cat <<'EOF'
Usage: ./scripts/setup-pi.sh [options]

  --host USER@HOST  Cross-build on this machine and install on the Pi
  --arch arm64|armv7  Target arch (default: arm64)
  --yes, -y         Non-interactive (DISCORD_TOKEN / DISCORD_APP_ID required)
  --skip-start      Install only; do not start systemd
  --remote-captcha  Run Chromium on the private Xvfb :99 display for HTTPS relay
  -h, --help        Show help

/auth needs no inbound port. Password login requires a Pi desktop session with
Chrome/Chromium; headless Pi installations should use Riot Mobile QR.

--remote-captcha is opt-in. It requires Xvfb and Chromium on the Pi and an
HTTPS AUTH_BASE_URL. Missing packages are reported; this script never installs
them automatically.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) HOST="${2:-}"; shift 2 ;;
    --arch)
      ARCH="${2:-}"
      shift 2
      case "$ARCH" in
        arm64|aarch64) ARCH=arm64 ;;
        armv7|armv7l|arm) ARCH=armv7 ;;
        *) echo "unsupported --arch $ARCH (use arm64 or armv7)" >&2; exit 1 ;;
      esac
      ;;
    --yes|-y) YES=1; shift ;;
    --skip-start) SKIP_START=1; shift ;;
    --remote-captcha) REMOTE_CAPTCHA=1; shift ;;
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

remote_captcha_dependencies() {
  local missing=0
  if [[ ! -x /usr/bin/Xvfb ]]; then
    echo "remote CAPTCHA dependency missing: Xvfb" >&2
    missing=1
  fi
  if ! command -v chromium >/dev/null 2>&1 && \
     ! command -v chromium-browser >/dev/null 2>&1 && \
     ! command -v google-chrome >/dev/null 2>&1; then
    echo "remote CAPTCHA dependency missing: Chromium" >&2
    missing=1
  fi
  if [[ "$missing" -ne 0 ]]; then
    echo "Install dependencies first: sudo apt-get update && sudo apt-get install -y xvfb chromium" >&2
    return 1
  fi
}

require_remote_captcha_https() {
  local auth_base="$1"
  if [[ "$auth_base" != https://* ]]; then
    echo "--remote-captcha requires HTTPS AUTH_BASE_URL (got ${auth_base})" >&2
    exit 1
  fi
}

detect_lan_ip() {
  if command -v hostname >/dev/null 2>&1; then
    local ip
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    if [[ -n "$ip" ]]; then
      echo "$ip"
      return
    fi
  fi
  echo "127.0.0.1"
}

write_env_file() {
  local dest="$1" auth_base="$2"
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
DATABASE_PATH=/var/lib/valorant-bot/data/bot.db
STORE_RESET_CRON=${STORE_RESET_CRON:-0 0 * * *}
EOF
}

if [[ -n "$HOST" ]]; then
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    ssh "$HOST" 'if [[ ! -x /usr/bin/Xvfb ]] || (! command -v chromium >/dev/null 2>&1 && ! command -v chromium-browser >/dev/null 2>&1 && ! command -v google-chrome >/dev/null 2>&1); then echo "remote CAPTCHA dependencies are missing" >&2; echo "Install dependencies first: sudo apt-get update && sudo apt-get install -y xvfb chromium" >&2; exit 1; fi'
  fi
  prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)"
  prompt DISCORD_APP_ID "Discord application ID (DISCORD_APP_ID)"
  if [[ "$ARCH" == armv7 ]]; then
    echo "building linux/armv7…"
    make build-pi32
    BIN="dist/valorant-bot-linux-armv7"
  else
    echo "building linux/arm64…"
    make build-pi
    BIN="dist/valorant-bot-linux-arm64"
  fi

  REMOTE_DIR="valorant-bot-deploy"
  scp -q "$BIN" "$HOST:/tmp/valorant-bot"
  ssh "$HOST" "mkdir -p ~/${REMOTE_DIR}"
  scp -q -r deploy scripts "$HOST:~/${REMOTE_DIR}/"

  # Prefer LAN IP detected on the Pi for /invite links.
  PI_IP="$(ssh "$HOST" 'hostname -I 2>/dev/null | awk "{print \$1}"' || true)"
  if [[ -z "$PI_IP" ]]; then
    PI_IP="127.0.0.1"
  fi
  AUTH_BASE_URL="${AUTH_BASE_URL:-http://${PI_IP}:8787}"
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    require_remote_captcha_https "$AUTH_BASE_URL"
  fi

  ENV_TMP="$(mktemp)"
  write_env_file "$ENV_TMP" "$AUTH_BASE_URL"
  scp -q "$ENV_TMP" "$HOST:/tmp/valorant.env"
  rm -f "$ENV_TMP"

  INSTALL_FLAGS=(--binary /tmp/valorant-bot --env /tmp/valorant.env)
  if [[ "$SKIP_START" -eq 1 ]]; then
    INSTALL_FLAGS+=(--skip-start)
  fi
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    INSTALL_FLAGS+=(--remote-captcha)
  fi
  ssh -t "$HOST" "cd ~/${REMOTE_DIR} && sudo ./deploy/install.sh ${INSTALL_FLAGS[*]}"

  echo
  echo "Pi setup complete on $HOST"
  echo "  AUTH_BASE_URL=${AUTH_BASE_URL}  (/invite only; /auth needs no inbound URL)"
  echo "  invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
  echo "  logs:   ssh $HOST 'sudo journalctl -u valorant-bot -f'"
  exit 0
fi

# Running on the Pi itself
if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root on the Pi: sudo $0" >&2
  echo "or from a laptop: $0 --host pi@<lan-ip>" >&2
  exit 1
fi

LAN_IP="$(detect_lan_ip)"
AUTH_BASE_URL="${AUTH_BASE_URL:-http://${LAN_IP}:8787}"
if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
  remote_captcha_dependencies
  require_remote_captcha_https "$AUTH_BASE_URL"
fi
prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)"
prompt DISCORD_APP_ID "Discord application ID (DISCORD_APP_ID)"
ENV_TMP="$(mktemp)"
write_env_file "$ENV_TMP" "$AUTH_BASE_URL"
INSTALL_FLAGS=(--env "$ENV_TMP")
if [[ "$SKIP_START" -eq 1 ]]; then
  INSTALL_FLAGS+=(--skip-start)
fi
if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
  INSTALL_FLAGS+=(--remote-captcha)
fi
./deploy/install.sh "${INSTALL_FLAGS[@]}"
rm -f "$ENV_TMP"

echo
echo "Pi setup complete"
echo "  AUTH_BASE_URL=${AUTH_BASE_URL}"
echo "  invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
echo "  logs:   journalctl -u valorant-bot -f"
