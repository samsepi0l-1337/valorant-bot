#!/usr/bin/env bash
# Install valorant-bot on Linux (Raspberry Pi or server) with systemd.
#
# Usage (from repo root, on the target machine):
#   sudo ./deploy/install.sh
#   sudo ./deploy/install.sh --binary dist/valorant-bot-linux-arm64
#   sudo ./deploy/install.sh --env deploy/env.pi.example
#
# Or copy a prebuilt binary from your Mac:
#   scp dist/valorant-bot-linux-arm64 pi@raspberrypi:~/valorant-bot
#   ssh pi@raspberrypi 'sudo ./deploy/install.sh --binary ~/valorant-bot'
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PREFIX=/usr/local
BIN_DST="${PREFIX}/bin/valorant-bot"
DATA_DIR=/var/lib/valorant-bot
ETC_DIR=/etc/valorant-bot
ENV_DST="${ETC_DIR}/env"
SERVICE_SRC="${ROOT}/deploy/valorant-bot.service"
SERVICE_DST=/etc/systemd/system/valorant-bot.service
USER_NAME=valorant
GROUP_NAME=valorant

BINARY=""
ENV_SRC=""
SKIP_START=0

usage() {
  cat <<'EOF'
Install Valorant Discord bot (systemd).

Options:
  --binary PATH   Binary to install (default: auto-detect dist/ or build native)
  --env PATH      Env template to copy if /etc/valorant-bot/env is missing
  --skip-start    Install only; do not systemctl enable --now
  -h, --help      Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BINARY="${2:-}"; shift 2 ;;
    --env) ENV_SRC="${2:-}"; shift 2 ;;
    --skip-start) SKIP_START=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1"; usage; exit 1 ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root: sudo $0 ..." >&2
  exit 1
fi

detect_binary() {
  local arch os
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  if [[ "$os" != "linux" ]]; then
    echo "install.sh is for Linux targets (got $os)" >&2
    exit 1
  fi
  case "$arch" in
    x86_64|amd64) echo "${ROOT}/dist/valorant-bot-linux-amd64" ;;
    aarch64|arm64) echo "${ROOT}/dist/valorant-bot-linux-arm64" ;;
    armv7l|armv6l) echo "${ROOT}/dist/valorant-bot-linux-armv7" ;;
    *) echo "" ;;
  esac
}

if [[ -z "$BINARY" ]]; then
  candidate="$(detect_binary)"
  if [[ -n "$candidate" && -x "$candidate" ]]; then
    BINARY="$candidate"
  elif [[ -x "${ROOT}/bin/valorant-bot" ]]; then
    BINARY="${ROOT}/bin/valorant-bot"
  elif [[ -x "${ROOT}/valorant-bot" ]]; then
    BINARY="${ROOT}/valorant-bot"
  else
    echo "no prebuilt binary found — building native binary..."
    if ! command -v go >/dev/null 2>&1; then
      echo "Go is required to build, or pass --binary PATH" >&2
      exit 1
    fi
    mkdir -p "${ROOT}/bin"
    (cd "$ROOT" && CGO_ENABLED=0 go build -o bin/valorant-bot ./cmd/bot)
    BINARY="${ROOT}/bin/valorant-bot"
  fi
fi

if [[ ! -f "$BINARY" ]]; then
  echo "binary not found: $BINARY" >&2
  exit 1
fi

if ! getent group "$GROUP_NAME" >/dev/null; then
  groupadd --system "$GROUP_NAME"
fi
if ! id -u "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --gid "$GROUP_NAME" --home-dir "$DATA_DIR" \
    --shell /usr/sbin/nologin "$USER_NAME"
fi

install -d -o "$USER_NAME" -g "$GROUP_NAME" -m 0750 "$DATA_DIR"
install -d -o "$USER_NAME" -g "$GROUP_NAME" -m 0750 "${DATA_DIR}/data"
install -d -m 0750 "$ETC_DIR"

install -m 0755 "$BINARY" "$BIN_DST"
install -m 0644 "$SERVICE_SRC" "$SERVICE_DST"

if [[ ! -f "$ENV_DST" ]]; then
  if [[ -z "$ENV_SRC" ]]; then
    arch="$(uname -m)"
    case "$arch" in
      aarch64|arm64|armv7l|armv6l) ENV_SRC="${ROOT}/deploy/env.pi.example" ;;
      *) ENV_SRC="${ROOT}/deploy/env.server.example" ;;
    esac
  fi
  if [[ ! -f "$ENV_SRC" ]]; then
    echo "env template missing: $ENV_SRC" >&2
    exit 1
  fi
  install -m 0640 -o root -g "$GROUP_NAME" "$ENV_SRC" "$ENV_DST"
  echo "wrote $ENV_DST from $ENV_SRC — edit secrets before start"
else
  echo "keeping existing $ENV_DST"
fi

systemctl daemon-reload

if [[ "$SKIP_START" -eq 1 ]]; then
  echo "installed. Edit $ENV_DST then: systemctl enable --now valorant-bot"
  exit 0
fi

if grep -Eq 'DISCORD_TOKEN=$|DISCORD_TOKEN=\s*$|BOT_SECRET=change-me' "$ENV_DST" 2>/dev/null; then
  echo "env still has placeholders — not starting."
  echo "  sudo nano $ENV_DST"
  echo "  sudo systemctl enable --now valorant-bot"
  exit 0
fi

systemctl enable --now valorant-bot
systemctl --no-pager --full status valorant-bot || true
echo "logs: journalctl -u valorant-bot -f"
