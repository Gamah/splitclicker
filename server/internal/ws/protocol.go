package ws

import "github.com/gamah/splitclicker/internal/game"

// Server→client frame shapes (JSON text). The hot-path frames are `armed` (out)
// and `click` (in); everything else is incidental. See PLAN.md §8.
//
// nonce is sent as a hex string, not a JSON number: it is an unguessable 64-bit
// token and JSON numbers can't carry full uint64 precision in every client.

type helloYou struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
}

type helloGame struct {
	Round   int    `json:"round"`
	Of      int    `json:"of"`
	Phase   string `json:"phase"`
	Players int    `json:"players"`
	Clicks  int    `json:"clicks"`
	ArmMin  int    `json:"arm_min"` // arming-window bounds, seconds (the delay itself is secret)
	ArmMax  int    `json:"arm_max"`
	// Bad-click penalty escalation (ms), so the client mirrors the live throttle
	// estimate without hardcoding the formula.
	PenaltyBase int `json:"penalty_base_ms"`
	PenaltyStep int `json:"penalty_step_ms"`
	// DevNote is the current host-editable broadcast note (empty = none), so a
	// mid-game joiner shows it without waiting for the next game's dev_note frame.
	DevNote string `json:"dev_note"`
	// TickMs is the live-window tick interval in ms (0 = ticking off), so the client
	// can size its pip jitter-buffer playback delay to the server cadence. Additive;
	// older clients ignore it.
	TickMs int `json:"tick_ms"`
}

// devNoteWire pushes the host-editable broadcast note. An empty note clears it
// on the client. Sent once per game (and whenever it changes between games).
type devNoteWire struct {
	T    string `json:"t"`
	Note string `json:"note"`
}

// testWire pushes an anticheat frame to a single sanctioned player, or clears it.
// state is the ladder rung: "test" (answer prompt, echoing id), "cooldown" or
// "ignored" (sidelined until until_ms — a timed cooldown / the bounty resolve
// time). message is the player-facing explanation. cleared=true tells the client
// to dismiss any overlay (they're back in play).
type testWire struct {
	T       string `json:"t"`
	State   string `json:"state"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Prompt  string `json:"prompt"`
	Message string `json:"message"`
	UntilMs int64  `json:"until_ms"`
	Cleared bool   `json:"cleared"`
}

// achievementWire tells the client to fire a manual achievement unlock by ident.
// It carries out-of-band feats the server detects off the game socket (e.g.
// poking the backend into a 404, fumbling the admin password) and matches back to
// a game client by IP — see Hub.FireAchievement.
type achievementWire struct {
	T     string `json:"t"`
	Ident string `json:"ident"`
}

// bountyUpdateWire nudges every client to re-fetch the bounty state (/config +
// /bounties/previous) because the active bounty just rolled over. It carries no
// data — the HTTP endpoints are the source of truth; this is only a "refresh now"
// signal, so a client never sits in the stale post-rollover state. Older clients
// ignore the unknown frame.
type bountyUpdateWire struct {
	T string `json:"t"`
}

type helloWire struct {
	T    string    `json:"t"`
	You  helloYou  `json:"you"`
	Game helloGame `json:"game"`
}

type pendingWire struct {
	T       string `json:"t"`
	Round   int    `json:"round"`
	Of      int    `json:"of"`
	Players int    `json:"players"`
	Clicks  int    `json:"clicks"`
	// Roster is the full {tag → username} map of connected non-legacy players,
	// broadcast at the arming stage so the client holds every name before any pip
	// can land (pips carry only the 4-byte tag; the client resolves the username
	// from here). Sent only to tick-capable (v5+) clients — see Hub.Pending — and
	// omitted (omitempty) otherwise. Knowingly O(M²); accepted for MVP (PLAN/§19).
	Roster []rosterEntry `json:"roster,omitempty"`
}

// rosterEntry is one player in the round_pending roster: their public tag (the
// pip identity) and current username (what the pip button shows).
type rosterEntry struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
}

// buttonWire is one live button in the v5 armed frame: its compact slot id (the wire
// handle the tick frames reference), its secret nonce (hex, echoed by a scoring click),
// and its server-RNG'd normalized position (0 = centre).
type buttonWire struct {
	ID    uint16 `json:"id"`
	Nonce string `json:"nonce"`
	X     int16  `json:"x"`
	Y     int16  `json:"y"`
}

// penalty_ms is this connection's own arm-delay penalty (the spam deterrent),
// surfaced so a masher can see they're being throttled. 0 for honest clients.
//
// Two shapes share this struct: tick-capable (v5+) clients get Buttons (the initial
// board; Nonce omitted) while below-v5 clients get the single persistent Nonce
// (Buttons omitted). The omitempty keeps each wire to just the fields its client uses.
type armedWire struct {
	T         string       `json:"t"`
	Round     int          `json:"round"`
	Seq       int          `json:"seq"`
	Nonce     string       `json:"nonce,omitempty"`
	Buttons   []buttonWire `json:"buttons,omitempty"`
	Players   int          `json:"players"`
	Clicks    int          `json:"clicks"`
	PenaltyMs int          `json:"penalty_ms"`
}

// youResult lets the client drive its `points` achievement stat exactly once:
// apply points_delta only for an unseen round_id (§7.1).
type youResult struct {
	PointsDelta int    `json:"points_delta"`
	RoundID     string `json:"round_id"`
}

type resultWire struct {
	T         string          `json:"t"`
	Round     int             `json:"round"`
	Of        int             `json:"of"`
	Winners   []game.Standing `json:"winners"`
	Standings []game.Standing `json:"standings"`
	You       youResult       `json:"you"`
}

// youGameOver drives placement/win achievements; game_id dedupes (§7.1).
// points_delta/round_id carry the FINAL round's score (the last round has no
// round_result of its own) so the client can drive its `points` stat once.
type youGameOver struct {
	Placement   int    `json:"placement"`
	Won         bool   `json:"won"`
	GameID      string `json:"game_id"`
	PointsDelta int    `json:"points_delta"`
	RoundID     string `json:"round_id"`
}

type gameOverWire struct {
	T         string          `json:"t"`
	Standings []game.Standing `json:"standings"`
	You       youGameOver     `json:"you"`
}
