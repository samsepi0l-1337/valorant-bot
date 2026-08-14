#!/usr/bin/env bash
# Locally-managed Cloudflare named tunnel for AUTH_PORT.
# Persistent origin: https://programtyping.dreamp.org
# Quick tunnels (./scripts/pi-tunnel.sh, ./scripts/run-local-remote.sh) are test-only.
#
# Does not start cloudflared unless --run is passed.
# Does not rewrite Discord secrets unless --write-env (uses write-remote-captcha-env.py).
#
# Usage:
#   cloudflared tunnel login          # once; pick the dreamp.org zone
#   ./scripts/named-tunnel.sh         # create/reuse tunnel, DNS, write config
#   ./scripts/named-tunnel.sh --write-env   # also persist AUTH_BASE_URL into .env
#   ./scripts/named-tunnel.sh --run         # start the named tunnel (bot on 127.0.0.1:8787)
#
# Overrides: NAMED_TUNNEL_NAME NAMED_TUNNEL_HOSTNAME AUTH_PORT CLOUDFLARED_DIR
#
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HELPER="${ROOT}/deploy/named-tunnel-config.py"
WRITER="${ROOT}/deploy/write-remote-captcha-env.py"
VALIDATOR="${ROOT}/deploy/validate-remote-captcha-origin.py"

NAME="${NAMED_TUNNEL_NAME:-valorant-bot}"
HOSTNAME="${NAMED_TUNNEL_HOSTNAME:-programtyping.dreamp.org}"
PORT="${AUTH_PORT:-8787}"
CLOUDFLARED_DIR="${CLOUDFLARED_DIR:-$HOME/.cloudflared}"
CERT="${CLOUDFLARED_DIR}/cert.pem"
CONFIG="${CLOUDFLARED_DIR}/valorant-bot.yml"
ENV_FILE="${ROOT}/.env"
WRITE_ENV=0
RUN=0
PRINT_ORIGIN=0

usage() {
  cat <<'EOF'
Locally-managed Cloudflare named tunnel for valorant-bot AUTH_PORT.

  cloudflared tunnel login
  ./scripts/named-tunnel.sh
  ./scripts/named-tunnel.sh --write-env [--env PATH]
  ./scripts/named-tunnel.sh --run
  ./scripts/named-tunnel.sh --print-origin

Default public origin: https://programtyping.dreamp.org
Upstream: http://127.0.0.1:8787 (WebSocket Upgrade enabled on HTTP ingress)
This script does not mint Riot captcha on the tunnel hostname.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --write-env)
      WRITE_ENV=1
      shift
      ;;
    --env)
      if [[ $# -lt 2 ]]; then
        echo "missing --env path" >&2
        exit 1
      fi
      ENV_FILE="$2"
      WRITE_ENV=1
      shift 2
      ;;
    --run)
      RUN=1
      shift
      ;;
    --print-origin)
      PRINT_ORIGIN=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 not found" >&2
  exit 1
fi

ORIGIN="$(python3 "${HELPER}" origin "${HOSTNAME}")"
SERVICE="$(python3 "${HELPER}" service "${PORT}")"
python3 "${HELPER}" tunnel-name "${NAME}" >/dev/null
python3 "${VALIDATOR}" "${ORIGIN}"

if [[ "${PRINT_ORIGIN}" -eq 1 ]]; then
  echo "${ORIGIN}"
  exit 0
fi

if ! command -v cloudflared >/dev/null 2>&1; then
  cat >&2 <<'EOF'
cloudflared not found.

Install (macOS Homebrew):
  brew install cloudflared

Install (Raspberry Pi OS / Debian arm64 example):
  curl -L --output cloudflared.deb \
    https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64.deb
  sudo dpkg -i cloudflared.deb

Then: cloudflared tunnel login
EOF
  exit 1
fi

if [[ ! -f "${CERT}" ]]; then
  cat >&2 <<EOF
Not logged in to Cloudflare (missing ${CERT}).

Run:
  cloudflared tunnel login

In the browser, authorize the dreamp.org zone (NS already points at Cloudflare).
cert.pem stays in ${CLOUDFLARED_DIR} — do not copy it into this repository.
Then re-run: ./scripts/named-tunnel.sh
EOF
  exit 1
fi

export TUNNEL_ORIGIN_CERT="${CERT}"
install -d -m 700 "${CLOUDFLARED_DIR}"

echo "Named tunnel ${NAME} → ${SERVICE}"
echo "Public hostname: ${HOSTNAME}"
echo "AUTH_BASE_URL=${ORIGIN}"
echo "Remote CAPTCHA: this named tunnel is the persistent origin. Quick tunnels remain test-only."
echo "WebSocket Upgrade is forwarded on HTTP ingress to loopback. Captcha tokens are not minted on ${HOSTNAME}."
echo

LIST="$(cloudflared tunnel list --output json --name "${NAME}")"
set +e
TUNNEL_ID="$(printf '%s\n' "${LIST}" | python3 "${HELPER}" parse-list "${NAME}")"
LIST_STATUS=$?
set -e
if [[ "${LIST_STATUS}" -eq 2 ]]; then
  echo "named tunnel list parse failed" >&2
  exit 1
fi
if [[ "${LIST_STATUS}" -eq 1 || -z "${TUNNEL_ID}" ]]; then
  echo "Creating named tunnel ${NAME}"
  cloudflared tunnel create --credentials-file "${CLOUDFLARED_DIR}/${NAME}.json" "${NAME}"
  LIST="$(cloudflared tunnel list --output json --name "${NAME}")"
  TUNNEL_ID="$(printf '%s\n' "${LIST}" | python3 "${HELPER}" parse-list "${NAME}")"
else
  echo "Reusing named tunnel ${NAME}"
fi
if [[ -z "${TUNNEL_ID}" ]]; then
  echo "named tunnel id missing" >&2
  exit 1
fi

CRED=""
for candidate in "${CLOUDFLARED_DIR}/${NAME}.json" "${CLOUDFLARED_DIR}/${TUNNEL_ID}.json"; do
  if [[ -f "${candidate}" ]]; then
    CRED="${candidate}"
    break
  fi
done
if [[ -z "${CRED}" ]]; then
  cat >&2 <<EOF
Tunnel ${NAME} exists but the credentials JSON is not on this machine.
Copy ${TUNNEL_ID}.json into ${CLOUDFLARED_DIR} (not into the git repo), then re-run.
EOF
  exit 1
fi

python3 "${HELPER}" render \
  --tunnel-id "${TUNNEL_ID}" \
  --credentials-file "${CRED}" \
  --hostname "${HOSTNAME}" \
  --service "${SERVICE}" \
  --output "${CONFIG}" \
  --repo-root "${ROOT}"

if cloudflared tunnel --config "${CONFIG}" ingress validate >/dev/null; then
  echo "Wrote ${CONFIG} (hostname → ${SERVICE}, WebSocket Upgrade enabled)"
else
  echo "named tunnel ingress validate failed" >&2
  exit 1
fi

set +e
ROUTE_OUT="$(cloudflared tunnel route dns "${NAME}" "${HOSTNAME}" 2>&1)"
ROUTE_STATUS=$?
set -e
if [[ "${ROUTE_STATUS}" -eq 0 ]]; then
  echo "Routed ${HOSTNAME} to named tunnel ${NAME}"
else
  printf '%s\n' "${ROUTE_OUT}" >&2
  cat >&2 <<EOF
Could not create CNAME ${HOSTNAME} → ${TUNNEL_ID}.cfargotunnel.com.
The Cloudflare account used at login must be able to edit DNS for dreamp.org.
EOF
  exit 1
fi

echo
echo "Set these on the bot host (do not mint Riot captcha on the tunnel hostname):"
echo "  AUTH_BASE_URL=${ORIGIN}"
echo "  AUTH_BIND_ADDRESS=127.0.0.1"
echo "  CAPTCHA_BROWSER_MODE=remote"
echo "  AUTH_PORT=${PORT}"
echo

if [[ "${WRITE_ENV}" -eq 1 ]]; then
  if [[ ! -f "${ENV_FILE}" ]]; then
    echo "missing env file" >&2
    exit 1
  fi
  python3 "${WRITER}" "${ENV_FILE}" "${ORIGIN}"
  echo "Updated AUTH_BASE_URL, AUTH_BIND_ADDRESS, and CAPTCHA_BROWSER_MODE in the env file (Discord secrets unchanged)."
fi

echo "Named tunnel is ready and not started by default."
echo "Start it after the bot is listening on ${SERVICE}:"
echo "  ./scripts/named-tunnel.sh --run"
echo "  # or: cloudflared tunnel --config ${CONFIG} run"

if [[ "${RUN}" -eq 1 ]]; then
  echo "Starting named tunnel ${NAME}"
  exec cloudflared tunnel --config "${CONFIG}" run
fi
