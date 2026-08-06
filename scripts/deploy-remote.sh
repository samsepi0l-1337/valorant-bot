#!/usr/bin/env bash
# Copy a prebuilt binary (+ deploy helpers) to a remote Linux host.
#
# Examples:
#   ./scripts/deploy-remote.sh pi@192.168.0.10 --pi
#   ./scripts/deploy-remote.sh user@vps.example.com --server
#   ./scripts/deploy-remote.sh pi@raspberrypi.local --pi --install
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

HOST=""
TARGET=""
DO_INSTALL=0

usage() {
  cat <<'EOF'
Usage: ./scripts/deploy-remote.sh USER@HOST --pi|--server [--install]

  --pi       build/upload linux-arm64 + Pi env template
  --server   build/upload linux-amd64 + server env template
  --install  ssh and run sudo deploy/install.sh on the host
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pi) TARGET=pi; shift ;;
    --server) TARGET=server; shift ;;
    --install) DO_INSTALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      if [[ -z "$HOST" ]]; then HOST="$1"; shift
      else echo "unknown: $1"; usage; exit 1
      fi
      ;;
  esac
done

if [[ -z "$HOST" || -z "$TARGET" ]]; then
  usage
  exit 1
fi

case "$TARGET" in
  pi)
    make build-pi
    BINARY=dist/valorant-bot-linux-arm64
    ENV_EX=deploy/env.pi.example
    ;;
  server)
    make build-linux
    BINARY=dist/valorant-bot-linux-amd64
    ENV_EX=deploy/env.server.example
    ;;
esac

REMOTE_DIR='~/valorant-bot-deploy'
ssh "$HOST" "mkdir -p $REMOTE_DIR/deploy"
scp "$BINARY" "$HOST:$REMOTE_DIR/$(basename "$BINARY")"
scp deploy/install.sh deploy/uninstall.sh deploy/valorant-bot.service \
  "$ENV_EX" "$HOST:$REMOTE_DIR/deploy/"
# install.sh expects repo-ish layout: ../deploy relative to ROOT; put binary next to a fake root
ssh "$HOST" "mkdir -p $REMOTE_DIR && mv $REMOTE_DIR/deploy/env.*.example $REMOTE_DIR/deploy/ 2>/dev/null; true"

echo "uploaded to $HOST:$REMOTE_DIR"
if [[ "$DO_INSTALL" -eq 1 ]]; then
  REMOTE_BIN="$REMOTE_DIR/$(basename "$BINARY")"
  REMOTE_ENV="$REMOTE_DIR/deploy/$(basename "$ENV_EX")"
  ssh -t "$HOST" "sudo $REMOTE_DIR/deploy/install.sh --binary $REMOTE_BIN --env $REMOTE_ENV --skip-start"
  echo "edit env on host, then: sudo systemctl enable --now valorant-bot"
else
  echo "next on host:"
  echo "  nano $REMOTE_DIR/deploy/$(basename "$ENV_EX")"
  echo "  sudo $REMOTE_DIR/deploy/install.sh --binary $REMOTE_DIR/$(basename "$BINARY") --env $REMOTE_DIR/deploy/$(basename "$ENV_EX")"
fi
