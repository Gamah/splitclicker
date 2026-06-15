#!/usr/bin/env bash
#
# restart.sh — restart the app to pick up config changes.
#
# Game tunables in data/config.json are read at startup, so restart the app after
# editing them. (The skin image and winner-lock time apply live and need no
# restart.) With --build, rebuilds the image first — use that after pulling new
# code; a plain restart is enough for config.json changes.
#
# Usage (run from server/):
#   ./scripts/restart.sh           # restart the running app container
#   ./scripts/restart.sh --build   # rebuild the image, then (re)start
set -euo pipefail

cd "$(dirname "$0")/.."   # server/

COMPOSE="docker compose -f docker/docker-compose.yml --env-file .env"

BUILD=0
for arg in "$@"; do
  case "$arg" in
    --build|-b) BUILD=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [ "$BUILD" -eq 1 ]; then
  GIT_HASH=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
    $COMPOSE up -d --build app
  echo "rebuilt and restarted app."
else
  $COMPOSE restart app
  echo "restarted app — config.json reloaded."
fi
