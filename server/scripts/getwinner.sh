#!/usr/bin/env bash
#
# getwinner.sh — print the current winner's Steam profile, but only once the
# winner-lock time has passed. The "winner" is the top of the sessions-won board
# (the player whose skin the HUD shows as the one to beat).
#
# Usage (run from server/):
#   ./scripts/getwinner.sh           # gated on the winner-lock time
#   ./scripts/getwinner.sh --force   # print even if the lock time hasn't passed
#
# Reads the lock time from the running app (same source the client uses, so the
# config.json/env precedence is already applied) and the winner from Postgres.
set -euo pipefail

cd "$(dirname "$0")/.."   # server/

APP_URL="${APP_URL:-http://127.0.0.1:6969}"
COMPOSE="docker compose -f docker/docker-compose.yml"

FORCE=0
for arg in "$@"; do
  case "$arg" in
    --force|-f) FORCE=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# Winner-lock time as epoch ms from GET /api/v1/config (0 = unset).
lock_ms=$(curl -fsS "$APP_URL/api/v1/config" 2>/dev/null \
  | grep -oE '"winner_lock_ms":[0-9]+' | grep -oE '[0-9]+$' || true)
lock_ms="${lock_ms:-0}"

if [ "$FORCE" -ne 1 ]; then
  if [ "$lock_ms" = "0" ]; then
    echo "No winner-lock time set (data/config.json winner_lock_time). Use --force to print anyway." >&2
    exit 1
  fi
  now_ms=$(( $(date +%s) * 1000 ))
  if [ "$now_ms" -lt "$lock_ms" ]; then
    echo "Winner isn't locked in yet — locks at $(date -d "@$(( lock_ms / 1000 ))")." >&2
    echo "Use --force to print the current leader anyway." >&2
    exit 1
  fi
fi

# Top of the sessions-won board, with their display name. Tab-separated so names
# with spaces survive the read.
row=$($COMPOSE exec -T postgres psql -U splitclicker -d splitclicker -At -F $'\t' -v ON_ERROR_STOP=1 -c \
  "SELECT w.steam_id, COALESCE(NULLIF(p.username,''), NULLIF(p.display_name,''), 'anonymous'), w.wins
     FROM session_wins w LEFT JOIN players p ON p.steam_id = w.steam_id
    ORDER BY w.wins DESC, w.steam_id ASC
    LIMIT 1;")

if [ -z "$row" ]; then
  echo "No sessions have been won yet — no winner to show." >&2
  exit 1
fi

IFS=$'\t' read -r steam_id name wins <<<"$row"
echo "Winner: $name ($wins session win(s))"
echo "https://steamcommunity.com/profiles/$steam_id"
