#!/usr/bin/env bash
#
# wipedb.sh — reset the competition boards so a fresh winner can be set.
#
# Truncates the score/win tables (hourly_scores, hourly_wins, finalized_hours,
# session_wins) AND the game-history tables (games, game_rounds, round_scores —
# which also back the admin recent-games and fastest-clickers views) in the
# running Postgres container, then restarts the app so its in-memory leaderboard
# cache reloads and the fastest_clickers materialized view re-refreshes from the
# now-empty tables. Player rows (claimed usernames / Steam identities) are KEPT
# unless --all is given.
#
# Usage (run from server/):
#   ./scripts/wipedb.sh            # wipe boards, keep players, confirm first
#   ./scripts/wipedb.sh --all      # also wipe the players table
#   ./scripts/wipedb.sh --yes      # skip the confirmation prompt
#
# Targets the docker-compose stack (docker/docker-compose.yml). For a non-Docker
# local DB, run the TRUNCATE below against $DATABASE_URL with psql yourself.
set -euo pipefail

cd "$(dirname "$0")/.."   # server/

COMPOSE="docker compose -f docker/docker-compose.yml"

WIPE_PLAYERS=0
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --all) WIPE_PLAYERS=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# round_scores/game_rounds/games are listed together so their FKs are satisfied
# within the one TRUNCATE (round_scores → game_rounds → games).
TABLES="hourly_scores, hourly_wins, finalized_hours, session_wins, round_scores, game_rounds, games"
if [ "$WIPE_PLAYERS" -eq 1 ]; then
  TABLES="$TABLES, players"
fi

echo "This will TRUNCATE: $TABLES"
echo "in the running splitclicker database (this is irreversible)."
if [ "$ASSUME_YES" -ne 1 ]; then
  read -r -p "Type 'wipe' to confirm: " confirm
  [ "$confirm" = "wipe" ] || { echo "aborted."; exit 1; }
fi

$COMPOSE exec -T postgres \
  psql -U splitclicker -d splitclicker -v ON_ERROR_STOP=1 \
  -c "TRUNCATE $TABLES RESTART IDENTITY;" \
  -c "REFRESH MATERIALIZED VIEW fastest_clickers;"

echo "boards wiped — restarting app so its leaderboard cache reloads empty…"
$COMPOSE restart app

echo "done. Edit data/config.json (skin_image / winner_lock_time) to set the next winner — applies live, no restart."
