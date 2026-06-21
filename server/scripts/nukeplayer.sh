#!/usr/bin/env bash
#
# nukeplayer.sh — permanently remove a single player from the database by SteamID.
#
# Deletes every row keyed on the player's steam_id across all tables, in FK-safe
# order, inside one transaction:
#   round_scores, anticheat_checks, anticheat_tests, anticheat_sanctions
#   (children FK'd to players, no ON DELETE CASCADE — must go first),
#   hourly_scores, hourly_wins, session_wins (board tables, no FK),
#   then the players row itself. Any bounties the player WON have their winner
#   unassigned (winner_id → NULL, winner_name/winner_wins cleared) so the FK is
#   satisfied and no ghost winner lingers — the bounty row + skin are kept.
# Afterwards the fastest_clickers materialized view is refreshed (it derives from
# round_scores) and the app is restarted so its in-memory leaderboard cache drops
# the player.
#
# This is irreversible. Intended for nuking a confirmed cheater / bad actor.
#
# Usage (run from server/):
#   ./scripts/nukeplayer.sh <steamid64>          # confirm first
#   ./scripts/nukeplayer.sh <steamid64> --yes    # skip the confirmation prompt
#   ./scripts/nukeplayer.sh <steamid64> --no-restart   # don't restart the app
#
# Targets the docker-compose stack (docker/docker-compose.yml). For a non-Docker
# local DB, run the same SQL against $DATABASE_URL with psql yourself.
set -euo pipefail

cd "$(dirname "$0")/.."   # server/

COMPOSE="docker compose -f docker/docker-compose.yml"

STEAMID=""
ASSUME_YES=0
RESTART=1
for arg in "$@"; do
  case "$arg" in
    --yes|-y) ASSUME_YES=1 ;;
    --no-restart) RESTART=0 ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *)
      if [ -n "$STEAMID" ]; then echo "unexpected extra arg: $arg" >&2; exit 2; fi
      STEAMID="$arg"
      ;;
  esac
done

# Same shape the server validates (SteamID64 = 1–20 digits; see api.steamIDRe).
if ! [[ "$STEAMID" =~ ^[0-9]{1,20}$ ]]; then
  echo "usage: $0 <steamid64> [--yes] [--no-restart]" >&2
  echo "  steamid must be 1–20 digits, got: '${STEAMID:-<missing>}'" >&2
  exit 2
fi

psql() {
  $COMPOSE exec -T postgres psql -U splitclicker -d splitclicker -At -F $'\t' -v ON_ERROR_STOP=1 "$@"
}

# The SteamID as a SQL string literal. psql's :'var' interpolation isn't reliably
# applied through `docker compose exec … -c`, so the (already digits-only
# validated) id is inlined directly — no injection surface.
SID="'$STEAMID'"

# Show who this is + how much they have on the boards, so a typo'd SteamID is
# obvious before anything is deleted.
summary=$(psql -c "
  SELECT COALESCE(NULLIF(p.username,''), NULLIF(p.display_name,''), '(no name)'),
         (SELECT COUNT(*)      FROM round_scores       WHERE steam_id = $SID),
         (SELECT COALESCE(SUM(points),0) FROM hourly_scores WHERE steam_id = $SID),
         (SELECT COALESCE(wins,0)        FROM session_wins  WHERE steam_id = $SID),
         (SELECT COUNT(*)      FROM bounties            WHERE winner_id = $SID)
    FROM players p
   WHERE p.steam_id = $SID;")

if [ -z "$summary" ]; then
  echo "No player with steam_id $STEAMID exists — nothing to do." >&2
  exit 1
fi
IFS=$'\t' read -r name n_scores total_points session_wins bounties_won <<<"$summary"

echo "About to PERMANENTLY delete this player (irreversible):"
echo "  steam_id:        $STEAMID"
echo "  name:            $name"
echo "  https://steamcommunity.com/profiles/$STEAMID"
echo "  scoring clicks:  $n_scores"
echo "  hourly points:   $total_points"
echo "  session wins:    $session_wins"
echo "  bounties won:    $bounties_won  (these will have their winner unassigned)"
if [ "$ASSUME_YES" -ne 1 ]; then
  read -r -p "Type 'nuke' to confirm: " confirm
  [ "$confirm" = "nuke" ] || { echo "aborted."; exit 1; }
fi

# One transaction: children first (FK, no cascade), board tables, unassign any
# won bounties, then the player row.
psql <<SQL
BEGIN;
DELETE FROM round_scores        WHERE steam_id = $SID;
DELETE FROM anticheat_checks    WHERE steam_id = $SID;
DELETE FROM anticheat_tests     WHERE steam_id = $SID;
DELETE FROM anticheat_sanctions WHERE steam_id = $SID;
DELETE FROM hourly_scores       WHERE steam_id = $SID;
DELETE FROM hourly_wins         WHERE steam_id = $SID;
DELETE FROM session_wins        WHERE steam_id = $SID;
UPDATE bounties
   SET winner_id = NULL, winner_name = '', winner_wins = 0
 WHERE winner_id = $SID;
DELETE FROM players             WHERE steam_id = $SID;
COMMIT;
SQL

# round_scores changed → the fastest_clickers leaderboard must re-derive.
psql -c "REFRESH MATERIALIZED VIEW fastest_clickers;" >/dev/null

echo "player $STEAMID ($name) removed."

if [ "$RESTART" -eq 1 ]; then
  echo "restarting app so its leaderboard cache drops the player…"
  $COMPOSE restart app
  echo "done."
else
  echo "skipped app restart (--no-restart): the in-memory leaderboard cache still"
  echo "holds the player until the next restart or cache refresh."
fi
