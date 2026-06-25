#!/bin/sh
# Interactive pre-build review of data/config.json, run by `make up`.
#
# Pure POSIX sh (no Go, no jq) so it works on a deploy host that only has Docker.
#   scripts/configure.sh          review the numeric game tunables (Enter keeps each)
#   scripts/configure.sh --skip   keep current/default values, no prompts
#
# Only the numeric startup tunables are reviewed; a rebuild/restart applies those.
# The live meta values (skin_image, winner_lock_time, dev_note, live_version) are
# re-read per request, so edit those directly in config.json; no rebuild needed.
#
# Any tunable missing from config.json (e.g. a key added after the file was created)
# is first seeded from the example default, so every key shows a real [current] value
# and pressing Enter always keeps exactly what is shown.
set -u

DIR="data"
CONF="$DIR/config.json"
EXAMPLE="$DIR/config.json.example"

SKIP=0
if [ "${1:-}" = "--skip" ]; then SKIP=1; fi

# Seed config.json from the committed example on first run.
if [ ! -f "$CONF" ]; then
	if [ ! -f "$EXAMPLE" ]; then
		echo "configure: $EXAMPLE not found (run from the server/ directory)" >&2
		exit 1
	fi
	cp "$EXAMPLE" "$CONF"
	echo "configure: seeded $CONF from example"
fi

if [ "$SKIP" -eq 1 ]; then
	echo "configure: --skip, keeping current values in $CONF"
	exit 0
fi

# Every numeric startup tunable (keep in sync with the env keys in internal/game/
# config.go and the example). The live meta values are reviewed separately, never here.
KEYS="arm_min_sec arm_max_sec clicks_per_player min_clicks rounds_per_game \
buttons_on_screen race_max_ms result_display_ms intermission_ms board_size \
tick_hz tick_sample_k penalty_base_ms penalty_step_ms fast_click_ms \
max_click_factor solo_lead_margin dominant_runner_up_min afk_cursor_min \
check_cooldown_threshold check_cooldown_mins check_ignore_after"

# getval KEY FILE -> KEY's numeric value in FILE (empty if the key is absent).
getval() {
	sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\([0-9][0-9.]*\).*/\1/p" "$2" | head -n1
}

# Seed any tunable missing from config.json with the example's default, so EVERY key
# has an explicit value to review (no blank brackets) and a typed change can never be
# lost to an absent key. Missing keys are inserted right after the opening brace.
for key in $KEYS; do
	[ -n "$(getval "$key" "$CONF")" ] && continue
	def=$(getval "$key" "$EXAMPLE")
	if [ -z "$def" ]; then
		echo "configure: no default for $key in $EXAMPLE (skipping)" >&2
		continue
	fi
	tmp="$CONF.tmp"
	if awk -v k="$key" -v v="$def" 'NR==1{print; print "  \"" k "\": " v ","; next} {print}' "$CONF" >"$tmp"; then
		mv "$tmp" "$CONF"
		echo "configure: added missing $key = $def (example default)"
	else
		rm -f "$tmp"
		echo "configure: failed to seed $key" >&2
	fi
done

echo "Reviewing $CONF. The value in [brackets] is current; press Enter to keep it, or type a new number to change it."
for key in $KEYS; do
	cur=$(getval "$key" "$CONF")
	printf "  %s [%s]: " "$key" "$cur"
	if ! read new; then new=""; fi
	if [ -z "$new" ]; then continue; fi
	# Accept only a plain non-negative number (one optional decimal point).
	case "$new" in
		*[!0-9.]* | *.*.* | .)
			echo "    '$new' is not a number, keeping $cur"
			continue
			;;
	esac
	tmp="$CONF.tmp"
	if sed "s|\(\"$key\"[[:space:]]*:[[:space:]]*\)[0-9][0-9.]*|\1$new|" "$CONF" >"$tmp"; then
		mv "$tmp" "$CONF"
	else
		rm -f "$tmp"
		echo "    failed to update $key" >&2
	fi
done
echo "configure: wrote $CONF"
