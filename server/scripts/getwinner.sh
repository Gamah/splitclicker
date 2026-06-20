#!/usr/bin/env bash
#
# getwinner.sh — print the winner of a skin bounty. The win time now lives in the
# DB (the bounties table), so this reads straight from Postgres — no config
# endpoint needed.
#
# Default: the most recently WON bounty's recorded winner (the player who won the
# most games during that bounty's window — locked in when its win time passed).
#
# --force: the CURRENT leader of the still-active bounty (most games won so far in
# its window), i.e. who would win if the deadline were now.
#
# Usage (run from server/):
#   ./scripts/getwinner.sh           # last settled bounty's winner
#   ./scripts/getwinner.sh --force   # active bounty's current leader
set -euo pipefail

cd "$(dirname "$0")/.."   # server/

COMPOSE="docker compose -f docker/docker-compose.yml"

FORCE=0
for arg in "$@"; do
  case "$arg" in
    --force|-f) FORCE=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

psql() {
  $COMPOSE exec -T postgres psql -U splitclicker -d splitclicker -At -F $'\t' -v ON_ERROR_STOP=1 -c "$1"
}

if [ "$FORCE" -ne 1 ]; then
  # The most recently settled bounty and its recorded winner.
  row=$(psql "
    SELECT winner_id,
           COALESCE(NULLIF(winner_name,''), 'anonymous'),
           winner_wins,
           COALESCE(NULLIF(label,''), '(unlabeled skin)'),
           won_at
      FROM bounties
     WHERE status = 'won' AND winner_id IS NOT NULL
     ORDER BY won_at DESC
     LIMIT 1;")
  if [ -z "$row" ]; then
    echo "No bounty has been won yet — use --force to see the active bounty's current leader." >&2
    exit 1
  fi
  IFS=$'\t' read -r steam_id name wins label won_at <<<"$row"
  echo "Winner of \"$label\" (settled $won_at UTC): $name — $wins game win(s) in the window"
  echo "https://steamcommunity.com/profiles/$steam_id"
  exit 0
fi

# --force: current leader of the active bounty's window so far.
row=$(psql "
  WITH active AS (
    SELECT label, activated_at, win_time FROM bounties WHERE status = 'active' LIMIT 1
  )
  SELECT gs.steam_id,
         COALESCE(NULLIF(p.username,''), NULLIF(p.display_name,''), 'anonymous'),
         COUNT(*),
         COALESCE(NULLIF(a.label,''), '(unlabeled skin)'),
         a.win_time
    FROM active a
    JOIN games g
      ON g.ended_at > a.activated_at
     AND g.ended_at <= LEAST(a.win_time, now())
    JOIN game_standings gs ON gs.game_id = g.id AND gs.placement = 1
    LEFT JOIN players p ON p.steam_id = gs.steam_id
   GROUP BY gs.steam_id, p.username, p.display_name, a.label, a.win_time
   ORDER BY COUNT(*) DESC, gs.steam_id ASC
   LIMIT 1;")

if [ -z "$row" ]; then
  echo "No active bounty with games won in its window yet." >&2
  exit 1
fi

IFS=$'\t' read -r steam_id name wins label win_time <<<"$row"
echo "Current leader for \"$label\" (locks $win_time UTC): $name — $wins game win(s) so far"
echo "https://steamcommunity.com/profiles/$steam_id"
