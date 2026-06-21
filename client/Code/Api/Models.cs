using System.Collections.Generic;
using System.Text.Json.Serialization;

namespace Splitclicker.Api;

// POST /api/v1/auth response: the public tag/username plus a single-use WS ticket
// (mint immediately before connecting; expires after ttl_ms).
// Username is the *claimed* handle only (empty when the player never set one) —
// persist/re-send only this, never DisplayName, or the Steam name gets bounced
// back as a username and 422s on reconnect. DisplayName is the resolved string
// to show (claimed handle, else Steam name, else hex tag).
public record AuthResponse(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username,
	[property: JsonPropertyName( "display_name" )] string DisplayName,
	[property: JsonPropertyName( "ticket" )] string Ticket,
	[property: JsonPropertyName( "ttl_ms" )] long TtlMs
);

// GET /api/v1/config response: server-driven bits the client needs at startup.
// WinnerLockMs is the countdown target as Unix epoch milliseconds (0 = unset) —
// a plain number so no date parsing is needed (the s&box sandbox doesn't
// whitelist System.Globalization). SkinUrl is the server path the current
// "skin to win" image is served from (made absolute against BaseUrl).
// InspectLink is the active bounty's CS2 inspect link, or "" when the skin is an
// uploaded image only. When set, the client decodes it locally and renders the
// live float / seed / name / wear bar, falling back to SkinUrl's image on failure.
public record ConfigResponse(
	[property: JsonPropertyName( "winner_lock_ms" )] long WinnerLockMs,
	[property: JsonPropertyName( "skin_url" )] string SkinUrl,
	[property: JsonPropertyName( "inspect_link" )] string InspectLink
);

// GET /api/v1/bounties/previous response row: a settled bounty + its winner, for
// the "previous winner" panel. SkinUrl is the per-bounty image fallback (made
// absolute against BaseUrl); InspectLink, when set, is decoded locally for the
// live skin render exactly like the current bounty. WinnerTag matches the client's
// own tag when the local player won (so the panel can say "you"); WinnerSteamId
// opens the profile; WonAtMs is epoch ms (no date parsing in the sandbox).
public record PreviousBounty(
	[property: JsonPropertyName( "id" )] long Id,
	[property: JsonPropertyName( "label" )] string Label,
	[property: JsonPropertyName( "skin_url" )] string SkinUrl,
	[property: JsonPropertyName( "inspect_link" )] string InspectLink,
	[property: JsonPropertyName( "winner_tag" )] string WinnerTag,
	[property: JsonPropertyName( "winner_steam_id" )] string WinnerSteamId,
	[property: JsonPropertyName( "winner_name" )] string WinnerName,
	[property: JsonPropertyName( "winner_wins" )] int WinnerWins,
	[property: JsonPropertyName( "won_at_ms" )] long WonAtMs
);

// One row of a leaderboard (GET /api/v1/leaderboard/*) and of the standings
// embedded in round_result / game_over. SteamId is the public SteamID64, used to
// open/copy the player's steamcommunity.com profile.
public record Standing(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username,
	[property: JsonPropertyName( "points" )] int Points,
	[property: JsonPropertyName( "steam_id" )] string SteamId,
	// Anticheat status for the active bounty: "live" / "cooldown" / "ignored".
	// Drives the coloured status dot on every board row. Empty ⇒ treated as live.
	[property: JsonPropertyName( "status" )] string Status
);

// --- WebSocket server→client frames (the "t" field selects the shape) ---

public record HelloYou(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username
);

public record HelloGame(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of,
	[property: JsonPropertyName( "phase" )] string Phase,
	[property: JsonPropertyName( "players" )] int Players,
	[property: JsonPropertyName( "clicks" )] int Clicks,
	[property: JsonPropertyName( "arm_min" )] int ArmMin,
	[property: JsonPropertyName( "arm_max" )] int ArmMax,
	[property: JsonPropertyName( "penalty_base_ms" )] int PenaltyBaseMs,
	[property: JsonPropertyName( "penalty_step_ms" )] int PenaltyStepMs,
	[property: JsonPropertyName( "dev_note" )] string DevNote,
	// Live-window tick interval in ms (0 = ticking off). Sizes the pip jitter-buffer
	// playback delay so opponent pips replay at their true relative moment.
	[property: JsonPropertyName( "tick_ms" )] int TickMs
);

public record HelloMsg(
	[property: JsonPropertyName( "you" )] HelloYou You,
	[property: JsonPropertyName( "game" )] HelloGame Game
);

// roster is the full {tag → username} map of connected players (v5+ only), sent at
// the arming stage so opponent pips — which carry only a 4-byte tag — can be
// labelled with a name. Absent on older servers/clients (null).
public record RosterEntry(
	[property: JsonPropertyName( "tag" )] string Tag,
	[property: JsonPropertyName( "username" )] string Username
);

public record PendingMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of,
	[property: JsonPropertyName( "players" )] int Players,
	[property: JsonPropertyName( "clicks" )] int Clicks,
	[property: JsonPropertyName( "roster" )] List<RosterEntry> Roster
);

// One live button in the v5 armed board: id is the compact slot handle (referenced
// by tick claim events), nonce the secret a scoring click on it must echo, x/y its
// server-placed normalized position (int16, 0 = centre). Same shape the tick frame's
// spawned replacements carry.
public record ButtonMsg(
	[property: JsonPropertyName( "id" )] int Id,
	[property: JsonPropertyName( "nonce" )] string Nonce,
	[property: JsonPropertyName( "x" )] int X,
	[property: JsonPropertyName( "y" )] int Y
);

// nonce is a hex string (an unguessable 64-bit token); echo it back verbatim in the
// click frame — never parse/reformat it. v5 servers send `buttons` (the initial
// multi-button board) and leave nonce empty; below-v5 servers send the single
// persistent `nonce` and no buttons. penalty_ms is this connection's own arm-delay
// penalty (0 for honest clients), surfaced so a masher sees the throttle.
public record ArmedMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "seq" )] int Seq,
	[property: JsonPropertyName( "nonce" )] string Nonce,
	[property: JsonPropertyName( "buttons" )] List<ButtonMsg> Buttons,
	[property: JsonPropertyName( "players" )] int Players,
	[property: JsonPropertyName( "clicks" )] int Clicks,
	[property: JsonPropertyName( "penalty_ms" )] int PenaltyMs
);

public record YouResult(
	[property: JsonPropertyName( "points_delta" )] int PointsDelta,
	[property: JsonPropertyName( "round_id" )] string RoundId
);

public record RoundResultMsg(
	[property: JsonPropertyName( "round" )] int Round,
	[property: JsonPropertyName( "of" )] int Of,
	[property: JsonPropertyName( "winners" )] List<Standing> Winners,
	[property: JsonPropertyName( "standings" )] List<Standing> Standings,
	[property: JsonPropertyName( "you" )] YouResult You
);

// points_delta/round_id carry the FINAL round's score (the last round folds into
// game_over with no round_result of its own) so the client can drive its `points`
// stat once, guarded by round_id like a normal round.
public record YouGameOver(
	[property: JsonPropertyName( "placement" )] int Placement,
	[property: JsonPropertyName( "won" )] bool Won,
	[property: JsonPropertyName( "game_id" )] string GameId,
	[property: JsonPropertyName( "points_delta" )] int PointsDelta,
	[property: JsonPropertyName( "round_id" )] string RoundId
);

public record GameOverMsg(
	[property: JsonPropertyName( "standings" )] List<Standing> Standings,
	[property: JsonPropertyName( "you" )] YouGameOver You
);

// note is the host-editable broadcast message (orange line under the throttle);
// empty clears it. Pushed once per game and carried in hello for mid-game joiners.
public record DevNoteMsg(
	[property: JsonPropertyName( "note" )] string Note
);

// An anticheat frame pushed to this client after it failed end-of-round checks.
// state is the ladder rung:
//   "test"     — answer prompt (echoing id) before the server will arm us again.
//   "cooldown" — sidelined until until_ms (a timed cooldown); no test to answer.
//   "ignored"  — sidelined until until_ms (the bounty's resolve time); no test.
// message is the player-facing explanation for every state; until_ms is the epoch
// ms the cooldown/ignored state ends (the client shows a countdown to it).
// cleared=true means we're back in play — dismiss any overlay. (an empty state is
// treated as "test".)
public record TestMsg(
	[property: JsonPropertyName( "state" )] string State,
	[property: JsonPropertyName( "id" )] string Id,
	[property: JsonPropertyName( "kind" )] string Kind,
	[property: JsonPropertyName( "prompt" )] string Prompt,
	[property: JsonPropertyName( "message" )] string Message,
	[property: JsonPropertyName( "until_ms" )] long UntilMs,
	[property: JsonPropertyName( "cleared" )] bool Cleared
);

// ident is a server-pushed manual achievement unlock for an out-of-band feat
// (e.g. poking the backend into a 404, fumbling the admin password); it must
// match an achievement defined in the s&box project.
public record AchievementMsg(
	[property: JsonPropertyName( "ident" )] string Ident
);
