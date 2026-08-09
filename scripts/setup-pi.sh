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
REMOTE_CAPTCHA_ORIGIN_VALIDATOR="${ROOT}/deploy/validate-remote-captcha-origin.py"
LOCAL_SETUP_TMP_DIR=""
REMOTE_SETUP_TMP_DIR=""

usage() {
  cat <<'EOF'
Usage: ./scripts/setup-pi.sh [options]

  --host USER@HOST  Cross-build on this machine and install on the Pi
  --arch arm64|armv7  Target arch (default: arm64)
  --yes, -y         Non-interactive (DISCORD_TOKEN / DISCORD_APP_ID required)
  --skip-start      Install only; do not start systemd
  --remote-captcha  Run Chromium on the private Xvfb :99 display for HTTPS relay
  -h, --help        Show help

/auth QR needs no inbound port. A headless Pi can use Riot Mobile QR, or the
explicit --remote-captcha HTTPS relay when its extra dependencies are ready.

--remote-captcha is opt-in. It requires Xvfb, Chromium, xauth, mcookie, Python
3, and one exact HTTPS AUTH_BASE_URL origin. Missing packages are reported;
this script never installs them automatically. Base Pi installs use
CAPTCHA_BROWSER_MODE=disabled (QR only); the --remote-captcha drop-in supplies
remote mode separately.
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
  local var="$1" label="$2" mode="${3:-plain}" value=""
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
  if [[ "$mode" == "secret" ]]; then
    if ! IFS= read -rs value; then
      printf '\n'
      return 1
    fi
    printf '\n'
  else
    IFS= read -r value
  fi
  printf -v "$var" '%s' "$value"
  export "$var"
}

cleanup_local_setup_tmp() {
  local directory="${LOCAL_SETUP_TMP_DIR:-}"
  if [[ -z "$directory" ]]; then
    return 0
  fi
  rm -f -- "${directory}/env"
  rmdir -- "$directory" 2>/dev/null || true
  LOCAL_SETUP_TMP_DIR=""
}

cleanup_remote_setup_tmp() {
  local directory="${REMOTE_SETUP_TMP_DIR:-}"
  if [[ -z "$directory" || -z "$HOST" ]]; then
    return 0
  fi
  ssh "$HOST" "rm -f -- '${directory}/valorant-bot' '${directory}/valorant.env'; rmdir -- '${directory}'" \
    >/dev/null 2>&1 || true
  REMOTE_SETUP_TMP_DIR=""
}

cleanup_setup_temps() {
  local exit_status=$?
  set +e
  cleanup_local_setup_tmp
  cleanup_remote_setup_tmp
  return "$exit_status"
}

create_local_setup_tmp() {
  local temp_root="${TMPDIR:-/tmp}"
  LOCAL_SETUP_TMP_DIR="$(mktemp -d "${temp_root%/}/valorant-bot-setup.XXXXXX")"
  chmod 0700 "$LOCAL_SETUP_TMP_DIR"
}

create_remote_setup_tmp() {
  REMOTE_SETUP_TMP_DIR="$(ssh "$HOST" 'umask 077; mktemp -d /tmp/valorant-bot-install.XXXXXX')"
  if [[ ! "$REMOTE_SETUP_TMP_DIR" =~ ^/tmp/valorant-bot-install\.[[:alnum:]]{6,}$ ]]; then
    REMOTE_SETUP_TMP_DIR=""
    echo "remote setup temporary directory creation failed" >&2
    return 1
  fi
}

trap cleanup_setup_temps EXIT

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
  if [[ ! -x /usr/bin/xauth ]]; then
    echo "remote CAPTCHA dependency missing: xauth" >&2
    missing=1
  fi
  if [[ ! -x /usr/bin/mcookie ]]; then
    echo "remote CAPTCHA dependency missing: mcookie (util-linux)" >&2
    missing=1
  fi
  if [[ ! -x /usr/bin/python3 ]]; then
    echo "remote CAPTCHA dependency missing: python3 (install-time origin validator)" >&2
    missing=1
  fi
  if [[ "$missing" -ne 0 ]]; then
    echo "Install dependencies first: sudo apt-get update && sudo apt-get install -y xvfb chromium xauth util-linux python3" >&2
    return 1
  fi
}

validate_remote_captcha_origin() {
  local auth_base="$1"
  if ! command -v python3 >/dev/null 2>&1; then
    echo "python3 is required to validate remote CAPTCHA AUTH_BASE_URL" >&2
    return 1
  fi
  python3 "$REMOTE_CAPTCHA_ORIGIN_VALIDATOR" "$auth_base"
}

validate_remote_target_origin() {
  local auth_base="$1"
  local validator_payload auth_base_payload
  local remote_bootstrap='import base64,sys; source=base64.b64decode(sys.argv.pop(1)); sys.argv[-1]=base64.b64decode(sys.argv[-1]).decode("utf-8"); exec(compile(source,"<remote-captcha-origin-validator>","exec"),{"__name__":"__main__"})'

  # Keep stdin attached to the forced remote TTY so sudo can prompt. The
  # deployment-owned validator and already-validated caller origin travel as
  # one-line base64 values rather than putting the URL itself in the command.
  validator_payload="$(python3 -c 'import base64,sys; sys.stdout.write(base64.b64encode(sys.stdin.buffer.read()).decode("ascii"))' \
    < "$REMOTE_CAPTCHA_ORIGIN_VALIDATOR")"
  auth_base_payload="$(printf '%s' "$auth_base" | \
    python3 -c 'import base64,sys; sys.stdout.write(base64.b64encode(sys.stdin.buffer.read()).decode("ascii"))')"
  if ! ssh -tt "$HOST" \
    "sudo /usr/bin/python3 -c '$remote_bootstrap' '$validator_payload' --target-env /etc/valorant-bot/env '$auth_base_payload'"; then
    echo "remote CAPTCHA target preflight failed" >&2
    return 1
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
AUTH_BIND_ADDRESS=${AUTH_BIND_ADDRESS:-127.0.0.1}
CAPTCHA_BROWSER_MODE=disabled
DATABASE_PATH=/var/lib/valorant-bot/data/bot.db
STORE_RESET_CRON=${STORE_RESET_CRON:-0 0 * * *}
EOF
}

if [[ -n "$HOST" ]]; then
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    AUTH_BASE_URL="${AUTH_BASE_URL:-}"
    validate_remote_captcha_origin "$AUTH_BASE_URL"
    ssh "$HOST" 'missing=0; [[ -x /usr/bin/Xvfb ]] || { echo "remote CAPTCHA dependency missing: Xvfb" >&2; missing=1; }; [[ -x /usr/bin/xauth ]] || { echo "remote CAPTCHA dependency missing: xauth" >&2; missing=1; }; [[ -x /usr/bin/mcookie ]] || { echo "remote CAPTCHA dependency missing: mcookie (util-linux)" >&2; missing=1; }; [[ -x /usr/bin/python3 ]] || { echo "remote CAPTCHA dependency missing: python3 (install-time origin validator)" >&2; missing=1; }; if ! command -v chromium >/dev/null 2>&1 && ! command -v chromium-browser >/dev/null 2>&1 && ! command -v google-chrome >/dev/null 2>&1; then echo "remote CAPTCHA dependency missing: Chromium" >&2; missing=1; fi; if [[ "$missing" -ne 0 ]]; then echo "Install dependencies first: sudo apt-get update && sudo apt-get install -y xvfb chromium xauth util-linux python3" >&2; exit 1; fi'
    validate_remote_target_origin "$AUTH_BASE_URL"
  fi
  prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)" secret
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
  create_remote_setup_tmp
  scp -q "$BIN" "$HOST:${REMOTE_SETUP_TMP_DIR}/valorant-bot"
  ssh "$HOST" "mkdir -p ~/${REMOTE_DIR}"
  scp -q -r deploy scripts "$HOST:~/${REMOTE_DIR}/"

  # Prefer LAN IP detected on the Pi for /invite links.
  PI_IP="$(ssh "$HOST" 'hostname -I 2>/dev/null | awk "{print \$1}"' || true)"
  if [[ -z "$PI_IP" ]]; then
    PI_IP="127.0.0.1"
  fi
  AUTH_BASE_URL="${AUTH_BASE_URL:-http://${PI_IP}:8787}"
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    validate_remote_captcha_origin "$AUTH_BASE_URL"
  fi

  create_local_setup_tmp
  ENV_TMP="${LOCAL_SETUP_TMP_DIR}/env"
  write_env_file "$ENV_TMP" "$AUTH_BASE_URL"
  scp -q "$ENV_TMP" "$HOST:${REMOTE_SETUP_TMP_DIR}/valorant.env"
  cleanup_local_setup_tmp

  INSTALL_FLAGS=(--binary "${REMOTE_SETUP_TMP_DIR}/valorant-bot" --env "${REMOTE_SETUP_TMP_DIR}/valorant.env")
  if [[ "$SKIP_START" -eq 1 ]]; then
    INSTALL_FLAGS+=(--skip-start)
  fi
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    INSTALL_FLAGS+=(--remote-captcha)
  fi
  ssh -t "$HOST" "cd ~/${REMOTE_DIR} && sudo ./deploy/install.sh ${INSTALL_FLAGS[*]}"
  cleanup_remote_setup_tmp

  echo
  echo "Pi setup complete on $HOST"
  if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
    echo "  remote CAPTCHA: stable public HTTPS AUTH_BASE_URL + WebSocket Tunnel/reverse proxy required"
    echo "  AUTH_BASE_URL=${AUTH_BASE_URL}"
  else
    echo "  CAPTCHA_BROWSER_MODE=disabled (QR only; no inbound port or tunnel needed)"
    echo "  AUTH_BASE_URL=${AUTH_BASE_URL}  (/invite only)"
  fi
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
  validate_remote_captcha_origin "$AUTH_BASE_URL"
  remote_captcha_dependencies
fi
prompt DISCORD_TOKEN "Discord bot token (DISCORD_TOKEN)" secret
prompt DISCORD_APP_ID "Discord application ID (DISCORD_APP_ID)"
create_local_setup_tmp
ENV_TMP="${LOCAL_SETUP_TMP_DIR}/env"
write_env_file "$ENV_TMP" "$AUTH_BASE_URL"
INSTALL_FLAGS=(--env "$ENV_TMP")
if [[ "$SKIP_START" -eq 1 ]]; then
  INSTALL_FLAGS+=(--skip-start)
fi
if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
  INSTALL_FLAGS+=(--remote-captcha)
fi
./deploy/install.sh "${INSTALL_FLAGS[@]}"
cleanup_local_setup_tmp

echo
echo "Pi setup complete"
if [[ "$REMOTE_CAPTCHA" -eq 1 ]]; then
  echo "  remote CAPTCHA: stable public HTTPS AUTH_BASE_URL + WebSocket Tunnel/reverse proxy required"
  echo "  AUTH_BASE_URL=${AUTH_BASE_URL}"
else
  echo "  CAPTCHA_BROWSER_MODE=disabled (QR only; no inbound port or tunnel needed)"
  echo "  AUTH_BASE_URL=${AUTH_BASE_URL}  (/invite only)"
fi
echo "  invite: https://discord.com/oauth2/authorize?client_id=${DISCORD_APP_ID}"
echo "  logs:   journalctl -u valorant-bot -f"
