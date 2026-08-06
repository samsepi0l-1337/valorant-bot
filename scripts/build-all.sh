#!/usr/bin/env bash
# Cross-compile release binaries into dist/.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
exec make build-all
